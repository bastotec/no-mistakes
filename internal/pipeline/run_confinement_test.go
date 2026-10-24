package pipeline

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runconfine"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic offline canaries: a confined run's dispatch boundary must send
// every confinable step into the isolation worker (proved here by the worker
// proxy being selected and refusing on a missing substrate - the fail-closed
// path), refuse delivery steps instead of using daemon credentials, and leave
// unconfined runs untouched.
type recordingStep struct {
	name    types.StepName
	calls   int
	onRun   func()
	outcome *StepOutcome
}

func (s *recordingStep) Name() types.StepName { return s.name }
func (s *recordingStep) Execute(*StepContext) (*StepOutcome, error) {
	s.calls++
	if s.onRun != nil {
		s.onRun()
	}
	if s.outcome != nil {
		return s.outcome, nil
	}
	return &StepOutcome{}, nil
}
func newConfinedRunContext(t *testing.T, workDir string) *StepContext {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	policy := runconfine.DefaultPolicy()
	digest := runconfine.PolicyDigest(policy, run.ID, "head")
	if err := database.EnableRunConfinement(run.ID, "head", digest); err != nil {
		t.Fatal(err)
	}
	return &StepContext{
		Ctx: context.Background(), Run: run, Repo: repo, WorkDir: workDir,
		DB: database, EvidenceDir: filepath.Join(t.TempDir(), "evidence", run.ID),
		Config: &config.Config{Agent: types.AgentDeck}, Agent: &fakeConfinedAgent{},
		Log: func(string) {}, LogChunk: func(string) {}, LogFile: func(string) {},
	}
}

type fakeConfinedAgent struct{}

func (a *fakeConfinedAgent) Name() string { return "deck" }
func (a *fakeConfinedAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, context.Canceled
}
func (a *fakeConfinedAgent) Close() error { return nil }
func TestConfinedDispatchSendsConfinableStepsToTheWorker(t *testing.T) {
	for _, name := range runconfine.ConfinableSteps() {
		t.Run(string(name), func(t *testing.T) {
			sctx := newConfinedRunContext(t, t.TempDir())
			e := &Executor{db: sctx.DB}
			native := &recordingStep{name: name, onRun: func() {
				t.Error("confinable step dispatched natively despite the confinement pin")
			}}
			proxied, confined := e.confinedStep(sctx, native)
			if !confined {
				t.Fatal("confinable step not recognized as confined")
			}
			if proxied.Name() != name {
				t.Fatalf("proxy lost step identity: %s", proxied.Name())
			}
			if _, err := proxied.Execute(sctx); err == nil {
				t.Fatal("worker proxy executed without the Linux isolation substrate; the offline policy must refuse instead")
			} else if !strings.Contains(err.Error(), "run-confinement") {
				t.Fatalf("refusal is not attributed to the confinement boundary: %v", err)
			}
			if native.calls != 0 {
				t.Fatal("native step executed")
			}
		})
	}
}
func TestConfinedDispatchRefusesDeliveryAndAdvisorySteps(t *testing.T) {
	sctx := newConfinedRunContext(t, t.TempDir())
	e := &Executor{db: sctx.DB}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI, types.StepJev} {
		t.Run(string(name), func(t *testing.T) {
			native := &recordingStep{name: name, onRun: func() {
				t.Error("offline-forbidden step dispatched; daemon credentials would be reachable")
			}}
			proxied, confined := e.confinedStep(sctx, native)
			if !confined {
				t.Fatalf("%s not routed by the confinement boundary", name)
			}
			outcome, err := proxied.Execute(sctx)
			if err != nil {
				t.Fatalf("refusal must be a skip, not an error: %v", err)
			}
			if !outcome.Skipped || !strings.Contains(outcome.SkipReason, "run-confinement-policy") {
				t.Fatalf("%s not skipped with an explicit reason: %+v", name, outcome)
			}
			if native.calls != 0 {
				t.Fatal("native step executed")
			}
		})
	}
}
func TestConfinedDispatchLeavesUnconfinedRunsNative(t *testing.T) {
	work := t.TempDir()
	database, err := db.Open(filepath.Join(work, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(work, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database, WorkDir: work}
	e := &Executor{db: database}
	step := &recordingStep{name: types.StepReview}
	dispatched, confined := e.confinedStep(sctx, step)
	if confined || dispatched != step {
		t.Fatal("unconfined run's dispatch was replaced")
	}
	if _, err := dispatched.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if step.calls != 1 {
		t.Fatal("native step did not run")
	}
}
func TestConfinedDispatchFailsClosedOnUnreadablePin(t *testing.T) {
	sctx := newConfinedRunContext(t, t.TempDir())
	if err := sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	e := &Executor{db: sctx.DB}
	step := &recordingStep{name: types.StepReview}
	dispatched, confined := e.confinedStep(sctx, step)
	if !confined {
		t.Fatal("unreadable pin silently degraded to a native dispatch")
	}
	if _, err := dispatched.Execute(sctx); err == nil || !strings.Contains(err.Error(), "run-confinement-policy") {
		t.Fatalf("unreadable pin did not fail closed: %v", err)
	}
}
func TestConfinedWorkerConfigCarriesNoCredentialsOrOverrides(t *testing.T) {
	// Mirrors the worker-side narrowing from the steps package: the serialized
	// config that crosses the boundary must not carry forge profiles,
	// credentials, or agent path overrides. This pins the host side of that
	// contract by asserting the daemon's own pinned policy stays offline.
	policy := runconfine.DefaultPolicy()
	if !policy.Offline {
		t.Fatal("pinned policy lost the offline posture")
	}
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "credential") || strings.Contains(string(payload), "token") {
		t.Fatalf("policy serialization leaked credential-shaped material: %s", payload)
	}
}
