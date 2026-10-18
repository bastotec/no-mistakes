package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const staleRisk = "The parser currently loses required records."

func publicationRecoveryFixture(t *testing.T) (*pipeline.StepContext, string) {
	t.Helper()
	return publicationRecoveryFixtureWithClassification(t, true)
}

func publicationRecoveryFixtureWithClassification(t *testing.T, mismatch bool) (*pipeline.StepContext, string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "fixture"}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Env, _ = fakeGH(t, "")
	sctx.Config.Test.Evidence.AttachMedia = false
	// This is a deterministic recorded run, not a live validation claim.
	review := `{"findings":[],"summary":"fix-only round completed","risk_level":"medium","risk_rationale":"` + staleRisk + `","round_cap":1,"resolved_by_fix":[{"id":"r1","severity":"error","description":"Restore required parser records","action":"auto-fix"}]}`
	test := `{"findings":[],"summary":"fixture","tested":["fixture scenario"],"testing_summary":"recorded fixture evidence","artifacts":[],"scenarios":[{"name":"fixture scenario","result":"pass","live":true,"evidence":"fixture recorded observation","reason":""}],"verdict":"go","tested_head_sha":"` + head + `"}`
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		row, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		status := types.StepStatusCompleted
		if name == types.StepPR {
			status = types.StepStatusRunning
		}
		if name == types.StepCI {
			status = types.StepStatusPending
		}
		if err := sctx.DB.UpdateStepStatus(row.ID, status); err != nil {
			t.Fatal(err)
		}
		switch name {
		case types.StepReview:
			if _, err := sctx.DB.InsertStepRound(row.ID, 1, "auto_fix", &review, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(row.ID, review); err != nil {
				t.Fatal(err)
			}
		case types.StepTest:
			if _, err := sctx.DB.InsertStepRound(row.ID, 1, "initial", &test, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(row.ID, test); err != nil {
				t.Fatal(err)
			}
		default:
			if name != types.StepPR && name != types.StepCI {
				if _, err := sctx.DB.InsertStepRound(row.ID, 1, "initial", nil, nil, 1); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(dir), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	var err error
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ag.validationFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		issues := []string{}
		if strings.Contains(opts.Prompt, staleRisk) {
			issues = append(issues, "risk rationale presents a repaired finding as a current defect")
		}
		out, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, RiskDispositionMismatch: mismatch && len(issues) > 0, Issues: issues})
		return &agent.Result{Output: out}, nil
	}
	// Exercise real pinning and the ordinary guard, then model executor's
	// terminal failure persistence. No test manufactures a recovery-only waiver.
	_, err = (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "refusing non-compliant PR publication") {
		t.Fatalf("ordinary guard error=%v", err)
	}
	rows, _ := sctx.DB.GetStepsByRun(sctx.Run.ID)
	for _, row := range rows {
		if row.StepName == types.StepPR {
			if e := sctx.DB.FailStep(row.ID, err.Error(), 1); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := sctx.DB.UpdateRunErrorStatus(sctx.Run.ID, "step pr failed: "+err.Error(), types.RunFailed); e != nil {
		t.Fatal(e)
	}
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sctx, dir
}

func TestPublicationRecoveryRefusesNonTerminalRun(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	sctx.Run.Status = types.RunRunning
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error=%v", err)
	}
}
func TestPublicationRecoveryRefusesFailureBeforePublication(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	rows, _ := sctx.DB.GetStepsByRun(sctx.Run.ID)
	for _, r := range rows {
		if r.StepName == types.StepTest {
			if err := sctx.DB.FailStep(r.ID, "test failed", 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "incomplete validation step test") {
		t.Fatalf("error=%v", err)
	}
}
func TestPublicationRecoveryRefusesMissingOrDifferentHead(t *testing.T) {
	for _, mode := range []string{"missing", "different"} {
		t.Run(mode, func(t *testing.T) {
			sctx, dir := publicationRecoveryFixture(t)
			if mode == "missing" {
				head := sctx.Run.HeadSHA
				if err := os.Remove(filepath.Join(dir, ".git", "objects", head[:2], head[2:])); err != nil {
					t.Fatal(err)
				}
			} else {
				gitCmd(t, dir, "checkout", "--detach", sctx.Run.BaseSHA)
			}
			if _, err := RecoverPublication(sctx); err == nil {
				t.Fatal("different or missing head accepted")
			}
		})
	}
}
func TestPublicationRecoveryRefusesDifferentTargetOrBase(t *testing.T) {
	for _, mode := range []string{"target", "base"} {
		t.Run(mode, func(t *testing.T) {
			sctx, _ := publicationRecoveryFixture(t)
			if mode == "target" {
				sctx.Repo.UpstreamURL = "https://github.com/other/repo"
			} else {
				base := "other"
				sctx.Run.PRBaseBranch = &base
			}
			if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "target branch/base") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
func TestPublicationRecoveryTruthfulPublicationAndSamePRIdempotence(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	env, log := fakeGH(t, "")
	env = append(env, "FAKE_CLI_PUBLICATION_STATE="+filepath.Join(t.TempDir(), "forge-state.json"), "FAKE_CLI_PR_HEAD_SHA="+sctx.Run.HeadSHA)
	sctx.Env = env
	before, _, _, err := publicationEvidence(sctx)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := RecoverPublication(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != "https://github.com/test/repo/pull/99" {
		t.Fatalf("outcome=%+v", outcome)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "pr create") != 1 || strings.Contains(string(data), staleRisk) || !strings.Contains(string(data), "Fixed without rereview") {
		t.Fatalf("publication commands/body:\n%s", data)
	}
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture forge persisted CreatePR; no test-side rewrite or injected
	// existing-PR response is needed for the retry.
	sctx.Env = env
	again, err := RecoverPublication(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.PRURL != outcome.PRURL {
		t.Fatalf("repeat=%+v", again)
	}
	data, _ = os.ReadFile(log)
	if strings.Count(string(data), "pr create") != 1 || strings.Contains(string(data), "pr edit") || strings.Contains(string(data), "\npush ") {
		t.Fatalf("repeat mutated publication:\n%s", data)
	}
	after, _, _, err := publicationEvidence(sctx)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) || sctx.Run.Status != types.RunFailed {
		t.Fatal("recovery rewrote recorded validation or revived run")
	}
}
func TestPublicationRecoveryOrdinaryGuardStillRefusesMisstatedDisposition(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	ag := sctx.Agent.(*mockAgent)
	// A recovery draft that repeats the same stale claim in its narrative must
	// still fail, even though the machine-generated risk section is repaired.
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"title":"fix: parser","body":"## What Changed\n\n` + staleRisk + `"}`)}, nil
	}
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "refusing non-compliant PR publication") {
		t.Fatalf("guard error=%v", err)
	}
}
func TestPublicationRecoveryRefusesMissingValidationEvidence(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	rows, _ := sctx.DB.GetStepsByRun(sctx.Run.ID)
	for _, r := range rows {
		if r.StepName == types.StepTest {
			if err := sctx.DB.ClearStepFindings(r.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := RecoverPublication(sctx); err == nil {
		t.Fatal("missing Test evidence accepted")
	}
}

func TestPublicationRecoveryRefusesUnclassifiedGuardFailure(t *testing.T) {
	sctx, _ := publicationRecoveryFixtureWithClassification(t, false)
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "ordinary guard refusal") {
		t.Fatalf("unclassified failure became recoverable: %v", err)
	}
}

func TestPublicationRecoveryRefusesDifferentExistingPR(t *testing.T) {
	for _, which := range []string{"head", "base"} {
		t.Run(which, func(t *testing.T) {
			sctx, _ := publicationRecoveryFixture(t)
			head, base := sctx.Run.HeadSHA, "main"
			if which == "head" {
				head = sctx.Run.BaseSHA
			} else {
				base = "other"
			}
			env, log := fakeGH(t, "")
			payload, _ := json.Marshal([]map[string]any{{"number": 99, "url": "https://github.com/test/repo/pull/99", "baseRefName": base, "headRefOid": head, "headRefName": "feature", "headRepository": map[string]string{"nameWithOwner": "test/repo"}}})
			sctx.Env = append(env, "FAKE_CLI_PR_LIST_JSON="+string(payload))
			if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "existing PR head or target/base differs") {
				t.Fatalf("error=%v", err)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "pr create") || strings.Contains(string(data), "pr edit") {
				t.Fatalf("refusal mutated forge:\n%s", data)
			}
		})
	}
}

func TestPublicationRecoveryRefusesMissingPinnedGateEvidence(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	if err := sctx.DB.SetRunGates(sctx.Run.ID, `[{"name":"extra-check","after":"test","command":"true"}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "missing validation step gate.test.extra-check") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublicationRecoveryClassifiedMismatchCannotPassWithEmptyIssues(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	sctx.Agent.(*mockAgent).validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, RiskDispositionMismatch: true, Issues: []string{}})
		return &agent.Result{Output: raw}, nil
	}
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "refusing non-compliant PR publication") {
		t.Fatalf("contradictory validator verdict passed: %v", err)
	}
}
