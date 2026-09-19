package steps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/jev"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// jevTestConfig builds an enabled advisory config pointing at the named key
// variable. Tests inject the httptest gateway via JevStep.endpointOverride.
func jevTestConfig() config.Jev {
	return config.Jev{
		Enabled:       true,
		GatewayKeyEnv: "JEV_TEST_GATEWAY_KEY",
		SecretsFile:   filepath.Join(os.TempDir(), "jev-test-no-such-secrets-file"),
		Model:         jev.DefaultModel,
		Timeout:       5 * time.Second,
		MaxDiffBytes:  64 * 1024,
		Threshold:     0.6,
	}
}

// jevGatewayStub answers every question with the fixed probabilities in
// fixed (question name -> probability; questions without an entry get 0.05)
// and records the request bodies and Authorization headers it saw.
type jevGatewayStub struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	fixed  map[string]float64
}

func (s *jevGatewayStub) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.mu.Unlock()

	answers := map[string]any{}
	questions, _ := body["questions"].(map[string]any)
	for name := range questions {
		p := 0.05
		if v, ok := s.fixed[name]; ok {
			p = v
		}
		answers[name] = map[string]any{"type": "boolean", "probability": p}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers})
}

func (s *jevGatewayStub) lastBody() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

func newJevGitRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init", "-b", "main")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	// The run worktree sits on the feature branch; the integration branch
	// (main) stays at the base commit, exactly like a real run.
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA = strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))
	return dir, baseSHA, headSHA
}

func newJevStepContext(t *testing.T, dir, baseSHA, headSHA string, cfg config.Jev) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev = cfg
	return sctx
}

func TestJevStep_DisabledByDefaultSkips(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())
	sctx.Config.Jev.Enabled = false

	outcome, err := (&JevStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("expected skipped outcome when disabled")
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatal("disabled outcome must not gate")
	}
	if !strings.Contains(outcome.SkipReason, "disabled") {
		t.Errorf("SkipReason = %q", outcome.SkipReason)
	}
}

func TestJevStep_MissingKeySkipsWithoutFailing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())

	outcome, err := (&JevStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a missing key must not fail the run: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("expected skipped outcome")
	}
	if !strings.Contains(outcome.SkipReason, "JEV_TEST_GATEWAY_KEY") {
		t.Errorf("SkipReason should name the key variable: %q", outcome.SkipReason)
	}
}

func TestJevStep_GeminiModelIsRefused(t *testing.T) {
	dir, baseSHA, headSHA := newJevGitRepo(t)
	cfg := jevTestConfig()
	cfg.Model = "google/gemini-2.5-pro"
	t.Setenv("JEV_TEST_GATEWAY_KEY", "some-key")
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, cfg)

	outcome, err := (&JevStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("expected skipped outcome for a Gemini model")
	}
	if !strings.Contains(outcome.SkipReason, "Gemini") {
		t.Errorf("SkipReason = %q", outcome.SkipReason)
	}
}

func TestJevStep_CompletesAdvisoryAndNeverGates(t *testing.T) {
	const key = "test-gateway-key-value"
	t.Setenv("JEV_TEST_GATEWAY_KEY", key)

	stub := &jevGatewayStub{fixed: map[string]float64{"breaking": 0.91, "security": 0.62, "bug": 0.11, "data_loss": 0.03}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome.Skipped {
		t.Fatalf("expected a completed evaluation, got skip: %q", outcome.SkipReason)
	}
	if outcome.NeedsApproval {
		t.Fatal("advisory outcome must never need approval")
	}
	if outcome.AutoFixable {
		t.Fatal("advisory outcome must never be auto-fixable")
	}
	if outcome.ExitCode != 0 {
		t.Errorf("ExitCode = %d", outcome.ExitCode)
	}

	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if types.HasAskUserFindings(findings) {
		t.Fatal("advisory findings must never contain ask-user items")
	}
	if len(types.AutoFixableFindings(findings).Items) != 0 {
		t.Fatal("advisory findings must never contain auto-fix items")
	}

	// Flagged (breaking, security) become items; clear (bug, data_loss) stay
	// summary-only.
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 finding items (flagged), got %d: %#v", len(findings.Items), findings.Items)
	}
	for _, item := range findings.Items {
		if item.Severity != types.FindingSeverityInfo {
			t.Errorf("item severity = %q, want info", item.Severity)
		}
		if item.Action != types.ActionNoOp {
			t.Errorf("item action = %q, want no-op", item.Action)
		}
		if item.Category != FindingCategoryJev {
			t.Errorf("item category = %q", item.Category)
		}
	}
	if !strings.Contains(findings.Summary, "flagged") {
		t.Errorf("summary should carry the flagged verdict: %q", findings.Summary)
	}
	if !strings.Contains(findings.Summary, "p=0.91") || !strings.Contains(findings.Summary, "p=0.11") {
		t.Errorf("summary should carry every probability: %q", findings.Summary)
	}

	// The evaluated state is the diff; the key never appears anywhere.
	body := stub.lastBody()
	if body == nil {
		t.Fatal("gateway saw no request")
	}
	state, _ := body["state"].(string)
	if !strings.Contains(state, "feature.txt") || !strings.Contains(state, "feature line") {
		t.Errorf("state should be the run diff, got: %.200q", state)
	}
	questions, _ := body["questions"].(map[string]any)
	if len(questions) != len(jev.AdvisoryQuestions()) {
		t.Errorf("gateway received %d questions, want %d", len(questions), len(jev.AdvisoryQuestions()))
	}
	for _, q := range jev.AdvisoryQuestions() {
		if _, ok := questions[q.Name]; !ok {
			t.Errorf("gateway did not receive question %q", q.Name)
		}
	}
	if strings.Contains(outcome.Findings, key) {
		t.Fatal("findings leak the gateway key")
	}
	if auth := stub.auth[len(stub.auth)-1]; auth != "Bearer "+key {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestJevStep_HalfThresholdFlaggedAnswerBecomesAnItem(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	stub := &jevGatewayStub{fixed: map[string]float64{"breaking": 0.50, "bug": 0.11, "data_loss": 0.03, "security": 0.05}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	dir, baseSHA, headSHA := newJevGitRepo(t)
	cfg := jevTestConfig()
	cfg.Threshold = 0.5
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, cfg)

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome.Skipped {
		t.Fatalf("expected a completed evaluation, got skip: %q", outcome.SkipReason)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if !strings.Contains(findings.Summary, "flagged") {
		t.Fatalf("summary should carry the flagged verdict: %q", findings.Summary)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("a flagged verdict must list its flagged question as an item, got %d: %#v", len(findings.Items), findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "flags a breaking change") {
		t.Errorf("item should flag the breaking question: %q", findings.Items[0].Description)
	}
}

func TestJevStep_GatewayFailureSkipsAndNeverFails(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("a gateway failure must not fail the run: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("expected skipped outcome on gateway failure")
	}
	if !strings.Contains(outcome.SkipReason, "jev unavailable") {
		t.Errorf("SkipReason = %q", outcome.SkipReason)
	}
}

func TestJevStep_EmptyDiffSkips(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()

	dir, baseSHA, _ := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, baseSHA, jevTestConfig())

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.Skipped || !strings.Contains(outcome.SkipReason, "no changes to evaluate") {
		t.Fatalf("expected no-changes skip, got %+v", outcome)
	}
	if called {
		t.Error("gateway was called for an empty diff")
	}
}

func TestJevStep_DiffTruncationIsVisible(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	stub := &jevGatewayStub{fixed: map[string]float64{"breaking": 0.9, "security": 0.1, "bug": 0.1, "data_loss": 0.1}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	dir, baseSHA, _ := newJevGitRepo(t)
	// Add a commit whose diff exceeds the tiny bound.
	big := strings.Repeat("line of substance\n", 500)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "big")
	headSHA := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	cfg := jevTestConfig()
	cfg.MaxDiffBytes = 2000
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, cfg)

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome.Skipped {
		t.Fatalf("expected completion, got skip: %q", outcome.SkipReason)
	}
	body := stub.lastBody()
	state, _ := body["state"].(string)
	if !strings.Contains(state, "[diff truncated:") {
		t.Errorf("state should carry the truncation marker: %.200q", state)
	}
	if !strings.Contains(outcome.Findings, "(diff truncated)") {
		t.Errorf("findings summary should report truncation: %q", outcome.Findings)
	}
}

func TestJevStep_UncertainVerdictFromGrayZone(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	stub := &jevGatewayStub{fixed: map[string]float64{"breaking": 0.48, "security": 0.1, "bug": 0.1, "data_loss": 0.1}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if !strings.Contains(findings.Summary, "uncertain") {
		t.Errorf("gray-zone probability should read uncertain, summary: %q", findings.Summary)
	}
	if !strings.Contains(findings.Summary, "uncertain p=0.48") {
		t.Errorf("summary should mark the gray-zone question: %q", findings.Summary)
	}
	if len(findings.Items) != 1 {
		t.Errorf("gray-zone question should appear as an item, got %d items", len(findings.Items))
	}
	if !strings.Contains(findings.Items[0].Description, "uncertain about") {
		t.Errorf("item description = %q", findings.Items[0].Description)
	}
}

func TestJevStep_UnansweredQuestionIsReportedNotGuessed(t *testing.T) {
	t.Setenv("JEV_TEST_GATEWAY_KEY", "k")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only one question answered at a clear probability.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{"breaking": map[string]any{"type": "boolean", "probability": 0.05}},
		})
	}))
	defer srv.Close()

	dir, baseSHA, headSHA := newJevGitRepo(t)
	sctx := newJevStepContext(t, dir, baseSHA, headSHA, jevTestConfig())

	outcome, err := (&JevStep{endpointOverride: srv.URL}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if !strings.Contains(findings.Summary, "not answered") {
		t.Errorf("summary should name unanswered questions: %q", findings.Summary)
	}
	if !strings.Contains(findings.Summary, "uncertain") {
		t.Errorf("missing answers must read uncertain, summary: %q", findings.Summary)
	}
}
