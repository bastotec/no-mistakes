package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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

func TestConfinedRunDispatchCarriesGateToWorker(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, dir, base, head, config.Commands{})
	step := &CustomGateStep{Gate: config.Gate{Name: "license-check", After: types.StepRebase, Command: "echo checked > gate-ran"}}
	proxy := confinedRunStep{step: step}
	if proxy.Name() != step.Name() {
		t.Fatal("proxy lost gate identity")
	}
	request := roundTripIsolatedRunInput(t, newIsolatedRunInput(sctx, step, "fixture", runconfine.DefaultPolicy(), "digest"))
	if err := validateIsolatedRunRequest(request); err != nil {
		t.Fatal(err)
	}
	workerStep, err := request.step()
	if err != nil {
		t.Fatal(err)
	}
	request.GateDir = sctx.GateDir
	workerCtx := request.stepContext(sctx.DB, sctx.Agent, nil, func(isolatedStepEvent) error { return nil })
	outcome, err := workerStep.Execute(workerCtx)
	if err != nil || outcome == nil || outcome.NeedsApproval {
		t.Fatalf("gate did not execute: %+v %v", outcome, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "gate-ran")); err != nil || strings.TrimSpace(string(data)) != "checked" {
		t.Fatalf("gate command did not run: %q %v", data, err)
	}
}

func roundTripIsolatedRunInput(t *testing.T, input isolatedRunInput) isolatedRunInput {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var request isolatedRunInput
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestIsolatedRunDispatchRefusesUnpinnedRun(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, t.TempDir(), "base", "head", config.Commands{})
	if _, _, err := runconfinePolicyForDispatch(sctx); err == nil || !strings.Contains(err.Error(), "run is not confined") {
		t.Fatalf("unpinned run dispatched a confined invocation: %v", err)
	}
}

func TestIsolatedRunDispatchRefusesMismatchedPolicyDigest(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, t.TempDir(), "base", "head", config.Commands{})
	policy := runconfine.DefaultPolicy()
	if err := sctx.DB.EnableRunConfinement(sctx.Run.ID, "head", runconfine.PolicyDigest(policy, sctx.Run.ID, "other-head")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runconfinePolicyForDispatch(sctx); err == nil || !strings.Contains(err.Error(), "does not match the policy digest") {
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
		AgentTimeout: time.Minute, TestAgentTimeout: 2 * time.Minute, ReviewAgentTimeout: 3 * time.Minute,
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
	if got.Commands.Test != "t" || !got.Test.Skip || got.PR.BaseBranch != "develop" || !got.DisableProjectSettings || got.ReviewAgentTimeout != 3*time.Minute {
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
	event := isolatedStepEvent{Kind: "invocation", Invocation: &db.AgentInvocation{Agent: "deck", Model: "fixture"}, Text: string(types.StepLint)}
	if err := applyIsolatedStepObservation(sctx, types.StepLint, event); err != nil {
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

func TestIsolatedRunDispatchKeepsLaunchPolicyAfterHeadAdvances(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, t.TempDir(), "base", "submitted", config.Commands{})
	digest := runconfine.PolicyDigest(runconfine.DefaultPolicy(), sctx.Run.ID, "submitted")
	if err := sctx.DB.EnableRunConfinement(sctx.Run.ID, "submitted", digest); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{"submitted", "review-fix", "test-fix", "documentation"} {
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
			t.Fatal(err)
		}
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		sctx.Run = run
		if _, got, err := runconfinePolicyForDispatch(sctx); err != nil || got != digest {
			t.Fatalf("policy changed after %s: %q %v", head, got, err)
		}
	}
}

func TestIsolatedRunReviewFixHonorsRoundCapAndBudget(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", head)
	ag := &mockAgent{name: "deck", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose != "review-fix" {
			t.Fatalf("unauthorized rereview: %s", opts.Purpose)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Minute || time.Until(deadline) < time.Minute {
			t.Fatalf("configured review budget lost: %v %v", deadline, ok)
		}
		if err := os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0600); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix the real bug"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing, sctx.FixWithoutRereview = true, true
	sctx.ReviewStartingHeadSHA = head
	sctx.Config.ReviewAgentTimeout = 2 * time.Minute
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"real bug","action":"auto-fix"}]}`
	request := roundTripIsolatedRunInput(t, newIsolatedRunInput(sctx, &ReviewStep{}, "fixture", runconfine.DefaultPolicy(), "digest"))
	request.GateDir = sctx.GateDir
	workerCtx := request.stepContext(sctx.DB, ag, json.NewDecoder(strings.NewReader("\"\"\n")), func(isolatedStepEvent) error { return nil })
	step, err := request.step()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := step.Execute(workerCtx)
	if err != nil || outcome == nil || len(ag.calls) != 1 || workerCtx.Run.HeadSHA == head || outcome.ReviewApprovedHeadSHA != "" {
		t.Fatalf("capped fix failed: %+v %v calls=%d", outcome, err, len(ag.calls))
	}
}

func TestIsolatedRunReviewReceivesUncertifiedProvenance(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	output, _ := json.Marshal(cleanReviewFindings())
	ag := &mockAgent{name: "deck", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: output}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID = base, head, "previous-run"
	priorFindings := `{"findings":[{"severity":"error","description":"previous reachable defect","action":"auto-fix"}]}`
	sctx.UncertifiedPriorRounds = []*db.StepRound{{Round: 1, FindingsJSON: &priorFindings, GlobalConfigYAML: []byte("operator-secret"), RepoConfigYAML: []byte("repository-secret")}}
	request := roundTripIsolatedRunInput(t, newIsolatedRunInput(sctx, &ReviewStep{}, "fixture", runconfine.DefaultPolicy(), "digest"))
	request.GateDir = sctx.GateDir
	if len(request.UncertifiedPriorRounds) != 1 || len(request.UncertifiedPriorRounds[0].GlobalConfigYAML) != 0 || len(request.UncertifiedPriorRounds[0].RepoConfigYAML) != 0 {
		t.Fatal("uncertified provenance carried private config into worker")
	}
	if string(sctx.UncertifiedPriorRounds[0].GlobalConfigYAML) != "operator-secret" {
		t.Fatal("request construction mutated host history")
	}
	workerCtx := request.stepContext(sctx.DB, ag, nil, func(isolatedStepEvent) error { return nil })
	if _, err := (&ReviewStep{}).Execute(workerCtx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("unexpected turns: %d", len(ag.calls))
	}
	for _, want := range []string{"Fix-round provenance:", base, head, "previous reachable defect"} {
		if !strings.Contains(ag.calls[0].Prompt, want) {
			t.Fatalf("emitted review prompt lost provenance %q: %s", want, ag.calls[0].Prompt)
		}
	}
	if workerCtx.UncertifiedSourceRunID != "previous-run" {
		t.Fatal("source run identity lost")
	}
}

func TestConfinedRunPublicationRefusesBeforeNativeOperations(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, t.TempDir(), "base", "head", config.Commands{})
	if err := sctx.DB.EnableRunConfinement(sctx.Run.ID, "head", "digest"); err != nil {
		t.Fatal(err)
	}
	if err := publishRunHead(sctx, "head", "", nil); err == nil || !strings.Contains(err.Error(), "offline runs cannot publish") {
		t.Fatalf("head publication did not refuse before Git: %v", err)
	}
	if _, err := PublishTestPreview(sctx); err == nil || !strings.Contains(err.Error(), "offline runs cannot publish") {
		t.Fatalf("preview did not refuse before forge lookup: %v", err)
	}
}

func TestConfinedReviewConfigKeepsSizingButDropsModelRoutes(t *testing.T) {
	src := &config.Config{Review: config.Review{MaxRounds: 2, Routes: config.ReviewRoutesRaw{Light: "other/light", Finders: []string{"other/finder"}, Verify: "other/verify"}}}
	for _, level := range []string{config.ReviewEffortLow, config.ReviewEffortHigh, config.ReviewEffortMax} {
		src.Review.Sizing.Fixed = level
		cfg := confinedRunConfig(src)
		plan := planReview(cfg.Review.Sizing, cfg.Review.Routes, nil, nil)
		ordinary := planReview(src.Review.Sizing, src.Review.Routes, nil, nil)
		if plan.Effort != level || plan.VerifyCap != ordinary.VerifyCap || len(plan.Dimensions) != len(ordinary.Dimensions) || cfg.Review.MaxRounds != 2 {
			t.Fatalf("review sizing changed: %+v", plan)
		}
		if plan.Tuning != nil && (plan.Tuning.Model != "" || plan.Tuning.Effort != ordinary.Tuning.Effort || plan.Tuning.MaxTurns != ordinary.Tuning.MaxTurns || plan.Tuning.DeadlineSecs != ordinary.Tuning.DeadlineSecs) {
			t.Fatalf("light tuning escaped the pinned model or changed bounds: %+v", plan.Tuning)
		}
		if len(cfg.Review.Routes.Finders) != 0 || plan.FinderTuning.Model != "" || plan.VerifyTuning.Model != "" || plan.FinderTuning.MaxTurns != ordinary.FinderTuning.MaxTurns || plan.VerifyTuning.MaxTurns != ordinary.VerifyTuning.MaxTurns {
			t.Fatalf("multi-angle tuning escaped the pinned model or changed bounds: %+v", plan)
		}
	}
	if src.Review.Routes.Light != "other/light" || len(src.Review.Routes.Finders) != 1 {
		t.Fatal("ordinary review routes mutated")
	}
}
