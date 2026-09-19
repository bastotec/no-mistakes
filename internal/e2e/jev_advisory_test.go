//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys drive the advisory Jev signal through the real daemon and
// the real pipeline executor, the way an operator experiences it: a global
// config block, a gate push, and the step's observable outcome in the run
// record, `axi status`, and `axi logs`.
//
// The gateway endpoint is the real https://ai-gateway.vercel.sh (the step
// has no endpoint override by design). The skip journeys authenticate with a
// deliberately bogus key, so every network call they cause is rejected and
// spends no quota; the one quota-spending journey (real answers) runs only
// when NM_JEV_E2E_REAL_KEY is set, mirroring the e2e-record precedent.

// jevStepOf returns the run's jev step result.
func jevStepOf(t *testing.T, run *ipc.RunInfo) ipc.StepResultInfo {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepJev)
	if !ok {
		t.Fatalf("run recorded no jev step; steps: %+v", run.Steps)
	}
	return step
}

// writeJevSecrets puts a parse-only secrets file in the daemon's isolated
// HOME so the step resolves the gateway key exactly the way an operator's
// ~/.secrets works.
func writeJevSecrets(t *testing.T, h *Harness, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.HomeDir, ".secrets"), []byte(content), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
}

// assertJevNeverLeaks scans every durable byte the product wrote for this
// run (logs, database, state) for the key value: the key is read by
// reference and must never be logged, stored, or rendered.
func assertJevNeverLeaks(t *testing.T, h *Harness, key string) {
	t.Helper()
	if key == "" {
		return
	}
	err := filepath.WalkDir(h.NMHome, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), key) {
			t.Fatalf("gateway key leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", h.NMHome, err)
	}
}

// TestJevAdvisoryDisabledByDefaultJourney pins the out-of-the-box behavior:
// no jev block in the global config means the signal is off, the pipeline
// records a ten-step run whose jev step skips with a reason naming the
// disabled config, and the run completes untouched.
func TestJevAdvisoryDisabledByDefaultJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/jev-disabled-default"
	// The pushed branch tries to enable the signal from its own repository
	// config; jev is global-only, so this must be inert.
	repoCfg := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\njev:\n  enabled: true\n  model: repo-steered/model\n"
	h.CommitChange(branch, ".no-mistakes.yaml", repoCfg, "try to enable jev from the pushed branch")
	h.CommitChange(branch, "jev-default.txt", "jev disabled by default\n", "add work")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	assertPipelineStepsInOrder(t, run.Steps)

	jev := jevStepOf(t, run)
	if jev.Status != types.StepStatusSkipped {
		t.Fatalf("jev step status = %s, want skipped", jev.Status)
	}
	if !strings.Contains(jev.SkipReason, "disabled") {
		t.Fatalf("jev skip reason should name the disabled global config, got %q", jev.SkipReason)
	}
	if strings.Contains(jev.SkipReason, "repo-steered") {
		t.Fatalf("repository config steered the global-only jev signal: %q", jev.SkipReason)
	}

	logsOut, err := h.Run("axi", "logs", "--run", run.ID, "--step", "jev")
	if err != nil {
		t.Fatalf("axi logs --step jev: %v\n%s", err, logsOut)
	}
	if !strings.Contains(logsOut, "disabled") {
		t.Fatalf("axi logs should render the jev skip reason:\n%s", logsOut)
	}
	t.Logf("axi status:\n%s", mustAxiStatus(t, h, run))
	t.Logf("axi logs --step jev:\n%s", logsOut)
}

// TestJevAdvisoryGeminiRefusedJourney: a configured Gemini model is refused
// outright, with the refusal visible as a skip reason naming the model, even
// though a key is present.
func TestJevAdvisoryGeminiRefusedJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		GlobalConfigExtra: "jev:\n  enabled: true\n  model: google/gemini-2.5-pro\n  gateway_key_env: NM_JEV_E2E_KEY",
	})
	writeJevSecrets(t, h, "export NM_JEV_E2E_KEY=nm-e2e-unused-key\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/jev-gemini-refused"
	h.CommitChange(branch, "jev-gemini.txt", "gemini refusal\n", "add work")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	jev := jevStepOf(t, run)
	if jev.Status != types.StepStatusSkipped {
		t.Fatalf("jev step status = %s, want skipped", jev.Status)
	}
	if !strings.Contains(jev.SkipReason, "Gemini") || !strings.Contains(jev.SkipReason, "gemini-2.5-pro") {
		t.Fatalf("jev skip reason should name the refused Gemini model, got %q", jev.SkipReason)
	}
	assertJevNeverLeaks(t, h, "nm-e2e-unused-key")
}

// TestJevAdvisoryUnavailableSignalSkipsJourney drives the fail-safe contract
// against the real gateway: the signal is enabled, the key resolves by
// reference from a parse-only secrets file, the gateway rejects the bogus
// key (or is unreachable), and the step answers with skip-and-report - a
// reason naming the failure in the step log and the run record - while the
// run proceeds and completes. The bogus key must never appear anywhere the
// product wrote.
func TestJevAdvisoryUnavailableSignalSkipsJourney(t *testing.T) {
	const bogusKey = "nm-e2e-bogus-gateway-key"
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		GlobalConfigExtra: "jev:\n  enabled: true\n  gateway_key_env: NM_JEV_E2E_KEY",
	})
	writeJevSecrets(t, h, "export NM_JEV_E2E_KEY="+bogusKey+"\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/jev-unavailable"
	h.CommitChange(branch, "jev-unavailable.txt", "signal unavailable\n", "add work")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	assertPipelineStepsInOrder(t, run.Steps)

	jev := jevStepOf(t, run)
	if jev.Status != types.StepStatusSkipped {
		t.Fatalf("jev step status = %s, want skipped", jev.Status)
	}
	if !strings.Contains(jev.SkipReason, "jev unavailable") {
		t.Fatalf("jev skip reason should name the unavailable signal, got %q", jev.SkipReason)
	}
	// The step after jev must still have run: a skip never blocks the pipeline.
	test, ok := findStep(run.Steps, types.StepTest)
	if !ok || test.Status != types.StepStatusCompleted {
		t.Fatalf("test step did not complete after the jev skip: %+v", test)
	}

	logPath := filepath.Join(h.NMHome, "logs", run.ID, "jev.log")
	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read durable jev log: %v", err)
	}
	// axi logs renders each line quoted/escaped, so the exact-match assertion
	// reads the durable log the step wrote; the rendered check below pins the
	// user-facing surface.
	if !strings.Contains(string(rawLog), jev.SkipReason) {
		t.Fatalf("durable jev log should carry the skip reason %q:\n%s", jev.SkipReason, rawLog)
	}
	logsOut, err := h.Run("axi", "logs", "--run", run.ID, "--step", "jev")
	if err != nil {
		t.Fatalf("axi logs --step jev: %v\n%s", err, logsOut)
	}
	if !strings.Contains(logsOut, "jev unavailable") {
		t.Fatalf("axi logs should render the jev skip reason:\n%s", logsOut)
	}
	assertJevNeverLeaks(t, h, bogusKey)
	t.Logf("jev skip reason: %s", jev.SkipReason)
	t.Logf("axi logs --step jev:\n%s", logsOut)
}

// TestJevAdvisoryRealGatewayJourney drives the live signal end-to-end with a
// real gateway key (NM_JEV_E2E_REAL_KEY), spending one evaluation's quota.
// It is skipped when the key is absent so the suite never spends money.
func TestJevAdvisoryRealGatewayJourney(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("NM_JEV_E2E_REAL_KEY"))
	if key == "" {
		t.Skip("NM_JEV_E2E_REAL_KEY not set; set it to a Vercel AI Gateway key to drive the live signal (spends one evaluation)")
	}
	t.Setenv("NM_JEV_E2E_KEY", key)

	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		GlobalConfigExtra: "jev:\n  enabled: true\n  gateway_key_env: NM_JEV_E2E_KEY",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/jev-live"
	h.CommitChange(branch, "jev-live.txt", "live advisory evaluation\n", "add work for the live jev evaluation")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 180*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	assertPipelineStepsInOrder(t, run.Steps)

	jev := jevStepOf(t, run)
	if jev.Status != types.StepStatusCompleted {
		t.Fatalf("jev step status = %s, want completed (skip reason %q)", jev.Status, jev.SkipReason)
	}
	if jev.FindingsJSON == nil {
		t.Fatal("completed jev step recorded no findings")
	}
	findings, err := types.ParseFindingsJSON(*jev.FindingsJSON)
	if err != nil {
		t.Fatalf("parse jev findings: %v", err)
	}
	if !strings.Contains(findings.Summary, "Jev advisory signal:") {
		t.Fatalf("findings summary should carry the advisory verdict, got %q", findings.Summary)
	}
	if !strings.Contains(findings.Summary, "p=") {
		t.Fatalf("findings summary should carry probabilities, got %q", findings.Summary)
	}
	for _, item := range findings.Items {
		if item.Severity != types.FindingSeverityInfo || item.Action != types.ActionNoOp {
			t.Fatalf("advisory item must be info/no-op, got %+v", item)
		}
	}

	logsOut, err := h.Run("axi", "logs", "--run", run.ID, "--step", "jev")
	if err != nil {
		t.Fatalf("axi logs --step jev: %v\n%s", err, logsOut)
	}
	if !strings.Contains(logsOut, "Jev advisory verdict:") {
		t.Fatalf("axi logs should render the advisory verdict:\n%s", logsOut)
	}
	assertJevNeverLeaks(t, h, key)
	t.Logf("jev findings summary: %s", findings.Summary)
	t.Logf("axi logs --step jev:\n%s", logsOut)
	t.Logf("axi status:\n%s", mustAxiStatus(t, h, run))
}

func mustAxiStatus(t *testing.T, h *Harness, run *ipc.RunInfo) string {
	t.Helper()
	out, err := h.Run("axi", "status", "--run", run.ID)
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, out)
	}
	return out
}
