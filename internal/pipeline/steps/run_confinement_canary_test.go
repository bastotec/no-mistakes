package steps

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runconfine"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Synthetic offline canaries for the whole-run confinement worker: the
// executor proxy must carry the wrapped step's identity and gate definition,
// an unpinned run must never dispatch a confined invocation, a mismatched
// policy digest must refuse, and the worker protocol must refuse a step the
// policy does not cover. The full namespace canaries (host secrets, sockets,
// egress) are owned by internal/testisolation and the e2e suite.

func TestConfinedRunProxyCarriesIdentityAndGateDefinition(t *testing.T) {
	proxy := confinedRunStep{step: &CustomGateStep{Gate: config.Gate{Name: "license-check", After: types.StepReview}}}
	if name := proxy.Name(); !name.IsCustomGate() || !strings.Contains(string(name), "license-check") {
		t.Fatalf("proxy lost the gate step's identity: %s", name)
	}
	gate := proxy.GateDefinition()
	if gate.Name != "license-check" || gate.After != types.StepReview {
		t.Fatalf("proxy lost the gate definition: %+v", gate)
	}
	core := confinedRunStep{step: &LintStep{}}
	if core.Name() != types.StepLint {
		t.Fatalf("proxy lost the core step's identity: %s", core.Name())
	}
	if gate := core.GateDefinition(); gate.Name != "" {
		t.Fatalf("core step produced a gate definition: %+v", gate)
	}
}

func TestIsolatedRunDispatchRefusesUnpinnedRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	source, base, head := setupGitRepo(t)
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, source, "worktree", "add", "--detach", work, head)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, work, base, head, config.Commands{})
	if _, err := executeIsolatedRunStep(sctx, &LintStep{}); err == nil || !strings.Contains(err.Error(), "run is not confined") {
		t.Fatalf("unpinned run dispatched a confined invocation: %v", err)
	}
}

func TestIsolatedRunDispatchRefusesMismatchedPolicyDigest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	source, base, head := setupGitRepo(t)
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, source, "worktree", "add", "--detach", work, head)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, work, base, head, config.Commands{})
	policy := runconfine.DefaultPolicy()
	// Pin against a different head than the run currently carries: the digest
	// must refuse rather than reinterpret the boundary for the new head.
	if err := sctx.DB.EnableRunConfinement(sctx.Run.ID, "0000000000000000000000000000000000000000", runconfine.PolicyDigest(policy, sctx.Run.ID, "0000000000000000000000000000000000000000")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeIsolatedRunStep(sctx, &LintStep{}); err == nil || !strings.Contains(err.Error(), "does not match the policy digest") {
		t.Fatalf("mismatched policy digest dispatched: %v", err)
	}
}

func TestIsolatedRunWorkerRefusesNonConfinableStep(t *testing.T) {
	request := isolatedRunInput{Run: &db.Run{ID: "r"}, StepName: "push", Policy: runconfine.DefaultPolicy()}
	err := validateIsolatedRunRequest(request)
	if err == nil || !strings.Contains(err.Error(), "is not confinable") {
		t.Fatalf("non-confinable step accepted by the worker: %v", err)
	}
	// A custom gate rides its anchor's confinement: a gate after review is
	// inside the boundary even though "gate.review.x" is not a core step name.
	request.StepName = "gate.review.license"
	request.Gate = &config.Gate{Name: "license", After: types.StepReview}
	if err := validateIsolatedRunRequest(request); err != nil {
		t.Fatalf("custom gate refused despite its anchor being confinable: %v", err)
	}
}

func TestIsolatedRunWorkerRefusesInvalidPolicy(t *testing.T) {
	request := isolatedRunInput{Run: &db.Run{ID: "r"}, StepName: "lint", Policy: runconfine.Policy{Version: 99, Scope: "all", Offline: true}}
	if err := validateIsolatedRunRequest(request); err == nil || !strings.Contains(err.Error(), "run-confinement-policy") {
		t.Fatalf("invalid policy accepted by the worker: %v", err)
	}
}

func TestConfinedRunConfigDropsCredentialsAndOverrides(t *testing.T) {
	src := &config.Config{
		Agent:          types.AgentClaude,
		Commands:       config.Commands{Prepare: "p", Test: "t", Lint: "l", Format: "f"},
		Test:           config.Test{Skip: true, SkipReason: "owned by CI"},
		Commit:         config.Commit{BranchPattern: "feature/*"},
		PR:             config.PR{BaseBranch: "develop"},
		IgnorePatterns: []string{"vendor/"}, ProtectedPaths: []string{"secrets/"},
		AgentTimeout: time.Minute, TestAgentTimeout: 2 * time.Minute,
		AgentConfig:            map[string]agentcfg.Profile{"deck": {Model: "fixture"}},
		DisableProjectSettings: true, Review: config.Review{MaxRounds: 2},
		Document: config.Document{Instructions: "policy"}, AutoFix: config.AutoFix{Lint: 3},
	}
	src.ForgeProfiles = config.ForgeProfiles{"github.com": {ExpectedLogin: "fleet"}}
	src.AgentPathOverride = map[string]string{"deck": "/usr/local/bin/deck"}
	got := confinedRunConfig(src)
	if got.Agent != types.AgentDeck {
		t.Fatalf("worker config did not pin the Deck adapter: %s", got.Agent)
	}
	if len(got.ForgeProfiles) != 0 || len(got.AgentPathOverride) != 0 {
		t.Fatal("worker config carried forge profiles or agent path overrides across the boundary")
	}
	if got.Commands.Test != "t" || !got.Test.Skip || got.PR.BaseBranch != "develop" || !got.DisableProjectSettings {
		t.Fatalf("worker config dropped a confinable field: %+v", got)
	}
}

func TestIsolatedRunEventNamesItsStep(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, t.TempDir(), "base", "head", config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepLint)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	event := isolatedRunEvent{Kind: "invocation", Invocation: &db.AgentInvocation{Agent: "deck", Model: "fixture"}, Text: string(types.StepLint)}
	if err := applyIsolatedRunObservation(sctx, event); err != nil {
		t.Fatal(err)
	}
	invocations, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 || invocations[0].StepName != string(types.StepLint) {
		t.Fatalf("invocation not recorded under the confined step's name: %+v", invocations)
	}
}
