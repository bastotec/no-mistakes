package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const staleRisk = "The parser currently loses required records."

func publicationRecoveryFixture(t *testing.T) (*pipeline.StepContext, string) {
	t.Helper()
	return publicationRecoveryFixtureWithClassification(t, true)
}

func publicationRecoveryFixtureWithClassification(t *testing.T, mismatch bool, configure ...func(*pipeline.StepContext)) (*pipeline.StepContext, string) {
	return publicationRecoveryClassifiedFixture(t, mismatch, false, configure...)
}

func preparePublicationRecoveryFixture(t *testing.T, privacy bool, configure ...func(*pipeline.StepContext)) (*pipeline.StepContext, string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "fixture"}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
	var err error
	sctx.Repo, err = sctx.DB.InsertRepo(dir, sctx.Repo.UpstreamURL, "main")
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run, err = sctx.DB.InsertRun(sctx.Repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Env, _ = fakeGH(t, "")
	sctx.Config.Test.Evidence.AttachMedia = false
	for _, fn := range configure {
		fn(sctx)
	}
	png := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
	pngJSON, _ := json.Marshal(png)
	// This is a deterministic recorded run, not a live validation claim.
	review := `{"findings":[],"summary":"fix-only round completed","risk_level":"medium","risk_rationale":"` + staleRisk + `","round_cap":1,"resolved_by_fix":[{"id":"r1","severity":"error","description":"Restore required parser records","action":"auto-fix"}]}`
	if privacy {
		review = `{"findings":[{"id":"repair-budget-review","category":"repair-budget","severity":"warning","action":"ask-user","description":"Automatic repair budget exhausted for review: consumed 2 repairs; approve or abort (run 01ARZ3NDEKTSV4RRFFQ69G5FAV)."}],"risk_level":"low","risk_rationale":"No code defects recorded"}`
	}
	test := `{"findings":[],"summary":"fixture","tested":["fixture scenario"],"testing_summary":"recorded fixture evidence","artifacts":[{"kind":"screenshot","label":"Recorded fixture","path":` + string(pngJSON) + `}],"scenarios":[{"name":"fixture scenario","result":"pass","live":true,"evidence":"fixture recorded observation","reason":""}],"verdict":"go","tested_head_sha":"` + head + `"}`
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepCI} {
		row, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		status := types.StepStatusCompleted
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
			if name != types.StepCI {
				if _, err := sctx.DB.InsertStepRound(row.ID, 1, "initial", nil, nil, 1); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(resolvePushURL(sctx)), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sctx, dir
}

func publicationRecoveryClassifiedFixture(t *testing.T, mismatch, privacy bool, configure ...func(*pipeline.StepContext)) (*pipeline.StepContext, string) {
	t.Helper()
	sctx, dir := preparePublicationRecoveryFixture(t, privacy, configure...)
	row, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepPR)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(row.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	ag := sctx.Agent.(*mockAgent)
	attempts := 0
	ag.validationFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		attempts++
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		issues := []string{}
		if strings.Contains(opts.Prompt, staleRisk) {
			issues = append(issues, "risk rationale presents a repaired finding as a current defect")
		}
		if privacy {
			issues = nil
			if attempts <= maxPRPublicationRepairs+1 {
				issues = []string{fmt.Sprintf("generated Review repair-budget text exposes an internal run identifier (verdict %d)", attempts)}
			}
		}
		out, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, RiskDispositionMismatch: !privacy && mismatch && len(issues) > 0, GeneratedHistoryPrivacy: privacy && len(issues) > 0, Issues: issues})
		return &agent.Result{Output: out}, nil
	}
	_, err = (&PRStep{mediaUploader: &stubMediaUploader{err: os.ErrPermission}}).Execute(sctx)
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

func TestPublicationRecoveryTerminalRefusalMatchesExecutorPersistence(t *testing.T) {
	for _, terminal := range []string{"privacy", "risk", "unclassified", "agent-error", "wrong-digest"} {
		t.Run(terminal, func(t *testing.T) {
			sctx, dir := preparePublicationRecoveryFixture(t, terminal == "privacy")
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			var attempts int
			ag := sctx.Agent.(*mockAgent)
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				attempts++
				pin, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
				if err != nil || pin == nil || pin.Refusal != nil {
					t.Fatalf("nonterminal attempt acquired refusal evidence: %+v %v", pin, err)
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					return nil, err
				}
				verdict := prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, GeneratedHistoryPrivacy: true, Issues: []string{fmt.Sprintf("earlier privacy refusal %d", attempts)}}
				if attempts == maxPRPublicationRepairs+1 {
					verdict.GeneratedHistoryPrivacy = terminal == "privacy"
					verdict.RiskDispositionMismatch = terminal == "risk"
					verdict.Issues = []string{"final " + terminal + " refusal https://user:secret@example.com/repo"}
					if terminal == "agent-error" {
						return nil, fmt.Errorf("validator transport failed")
					}
					if terminal == "wrong-digest" {
						verdict.ContentSHA256 = "not-the-final-content"
					}
				}
				output, err := json.Marshal(verdict)
				return &agent.Result{Output: output}, err
			}
			env, log := fakeGH(t, "")
			executor := pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{
				reconcileEnvStep{step: &PRStep{}, env: env},
			}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err == nil {
				t.Fatal("publication unexpectedly passed")
			}
			if attempts != maxPRPublicationRepairs+1 {
				t.Fatalf("validator attempts=%d", attempts)
			}
			var err error
			sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || sctx.Run.Status != types.RunFailed {
				t.Fatalf("terminal run=%+v error=%v", sctx.Run, err)
			}
			pin, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
			if err != nil || pin == nil {
				t.Fatalf("missing prospective pin: %+v %v", pin, err)
			}
			classified := terminal == "privacy" || terminal == "risk"
			if classified {
				if pin.Refusal == nil || sctx.Run.Error == nil || *sctx.Run.Error != "step pr failed: "+*pin.Refusal {
					t.Fatalf("terminal refusal mismatch: pin=%+v run=%+v", pin, sctx.Run)
				}
				if !strings.Contains(*pin.Refusal, "final "+terminal+" refusal") || !strings.Contains(*pin.Refusal, "sanitize-and-retry attempts") || strings.Contains(*pin.Refusal, "earlier privacy refusal") || strings.Contains(*pin.Refusal, "secret") {
					t.Fatalf("incorrect terminal refusal: %s", *pin.Refusal)
				}
				rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.StepName == types.StepPR && (row.Status != types.StepStatusFailed || row.Error == nil || *row.Error != *pin.Refusal) {
						t.Fatalf("step refusal mismatch: %+v", row)
					}
				}
			} else if pin.Refusal != nil {
				t.Fatalf("unrelated terminal failure gained recovery evidence: %s", *pin.Refusal)
			}
			_, err = ValidatePublicationRecoveryRecords(sctx)
			if classified && err != nil || !classified && err == nil {
				t.Fatalf("recovery eligibility for %s: %v", terminal, err)
			}
			commands, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "pr create") || strings.Contains(string(commands), "pr edit") {
				t.Fatalf("refused content reached forge write: %s", commands)
			}
		})
	}
}

func TestOrdinaryPublicationResumeRetainsPinsAndUsesCurrentPolicy(t *testing.T) {
	for _, compliant := range []bool{true, false} {
		name := "guard-refuses"
		if compliant {
			name = "publishes"
		}
		t.Run(name, func(t *testing.T) {
			sctx, dir := publicationRecoveryFixture(t)
			original, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
			if err != nil {
				t.Fatal(err)
			}
			_, _, evidence, err := publicationEvidence(sctx)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "checkout", "main")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "advance publication policy")
			policy := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "feature")
			if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "target branch/base") {
				t.Fatalf("recovery accepted changed policy: %v", err)
			}
			env, log := fakeGH(t, "")
			sctx.Env = env
			ag := sctx.Agent.(*mockAgent)
			drafted, validated := false, false
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				drafted = true
				if !strings.Contains(opts.Prompt, policy) {
					t.Fatalf("draft did not receive current policy: %s", opts.Prompt)
				}
				return &agent.Result{Output: []byte(`{"title":"fix: parser","body":"## What Changed\n\n- Restore records"}`)}, nil
			}
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				validated = true
				if !strings.Contains(opts.Prompt, "- pr_format_policy_sha: "+policy) {
					t.Fatalf("guard did not receive current policy: %s", opts.Prompt)
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					return nil, err
				}
				issues := []string{}
				if !compliant {
					issues = append(issues, "risk rationale contradicts recorded dispositions")
				}
				raw, err := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, RiskDispositionMismatch: !compliant, Issues: issues})
				return &agent.Result{Output: raw}, err
			}
			sctx.Run.Status = types.RunRunning
			outcome, err := (&PRStep{}).Execute(sctx)
			if compliant {
				if err != nil || outcome == nil || outcome.PRURL != "https://github.com/test/repo/pull/99" {
					t.Fatalf("ordinary resume failed: outcome=%+v error=%v", outcome, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "refusing non-compliant PR publication") {
				t.Fatalf("ordinary guard bypassed: %v", err)
			}
			if !drafted || !validated {
				t.Fatalf("ordinary publication skipped draft or guard: draft=%v guard=%v", drafted, validated)
			}
			after, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
			if err != nil || after == nil || after.JSON != original.JSON || after.Refusal == nil || *after.Refusal != *original.Refusal {
				t.Fatalf("ordinary resume changed original pins: %+v %v", after, err)
			}
			_, _, afterEvidence, err := publicationEvidence(sctx)
			if err != nil || afterEvidence != evidence {
				t.Fatalf("ordinary resume changed validation evidence: %v", err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "pr create") != compliant || strings.Contains(string(data), "pr edit") {
				t.Fatalf("unexpected ordinary publication operations: %s", data)
			}
			sctx.Run.Status = types.RunFailed
			if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "target branch/base") {
				t.Fatalf("ordinary resume promoted changed recovery pins: %v", err)
			}
		})
	}
}

func TestPublicationRecoveryInterruptedCreatePersistsSamePR(t *testing.T) {
	for _, state := range []scm.PRState{scm.PRStateOpen, scm.PRStateClosed, scm.PRStateMerged} {
		t.Run(string(state), func(t *testing.T) {
			testPublicationRecoveryInterruptedCreate(t, state)
		})
	}
}

func testPublicationRecoveryInterruptedCreate(t *testing.T, lifecycle scm.PRState) {
	t.Helper()
	sctx, dir := publicationRecoveryFixture(t)
	env, log := fakeGH(t, "")
	state := filepath.Join(t.TempDir(), "forge-state.json")
	body := filepath.Join(t.TempDir(), "pr-body.md")
	sctx.Env = append(env, "FAKE_CLI_PUBLICATION_STATE="+state, "FAKE_CLI_PR_BODY_FILE="+body, "FAKE_CLI_PR_HEAD_SHA="+sctx.Run.HeadSHA, "FAKE_CLI_PR_CREATE_ERR=response interrupted", "FAKE_CLI_PR_STATE="+string(lifecycle))
	originalRun := *sctx.Run
	originalRefs := gitCmd(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	originalPins, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	_, _, evidence, err := publicationEvidence(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "response interrupted") || outcome != nil {
		t.Fatalf("expected interrupted create: outcome=%+v error=%v", outcome, err)
	}
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || sctx.Run.PRURL != nil {
		t.Fatalf("interrupted create already associated: %+v %v", sctx.Run, err)
	}
	originalPR, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	originalBody, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	ag := sctx.Agent.(*mockAgent)
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("existing PR retry invoked draft agent")
		return nil, nil
	}
	ag.validationFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("existing PR retry invoked content validator")
		return nil, nil
	}
	for _, fault := range []string{"FAKE_CLI_PR_STATE_ERR=state unavailable", "FAKE_CLI_PR_STATE=UNKNOWN"} {
		env := sctx.Env
		sctx.Env = append(append([]string(nil), env...), fault)
		outcome, err := RecoverPublication(sctx)
		sctx.Env = env
		if err == nil || outcome != nil {
			t.Fatalf("unreadable lifecycle reported success: fault=%s outcome=%+v error=%v", fault, outcome, err)
		}
		sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || sctx.Run.PRURL != nil || sctx.Run.PRState == nil || *sctx.Run.PRState != "none" || sctx.Run.PRStateObservedAt != nil {
			t.Fatalf("unreadable lifecycle manufactured PR state: %+v %v", sctx.Run, err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		outcome, err := RecoverPublication(sctx)
		if err != nil || outcome == nil || outcome.PRURL != "https://github.com/test/repo/pull/99" {
			t.Fatalf("retry %d: outcome=%+v error=%v", attempt, outcome, err)
		}
		sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || sctx.Run.PRURL == nil || *sctx.Run.PRURL != outcome.PRURL || sctx.Run.Status != types.RunFailed {
			t.Fatalf("retry did not retain terminal run and PR association: %+v %v", sctx.Run, err)
		}
		if sctx.Run.PRState == nil || *sctx.Run.PRState != strings.ToLower(string(lifecycle)) {
			t.Fatalf("retry did not persist actual provider lifecycle %s: %+v", lifecycle, sctx.Run)
		}
		if sctx.Run.Error == nil || *sctx.Run.Error != *originalRun.Error || sctx.Run.HeadSHA != originalRun.HeadSHA || sctx.Run.BaseSHA != originalRun.BaseSHA {
			t.Fatalf("retry changed original validation verdict or heads: %+v", sctx.Run)
		}
		cached := (&branchsync.Service{DB: sctx.DB, Repo: sctx.Repo, WorkDir: dir}).InspectCached(sctx.Ctx)
		if cached.Pipeline.RunID != sctx.Run.ID || cached.PRState != strings.ToLower(string(lifecycle)) {
			t.Fatalf("branch sync lost recovered lifecycle: %+v", cached)
		}
		if lifecycle == scm.PRStateMerged && cached.Safety != "blocked_merged" {
			t.Fatalf("branch sync did not retire merged publication: %+v", cached)
		}
		if lifecycle == scm.PRStateClosed && cached.Safety != "blocked_closed" {
			t.Fatalf("branch sync did not retire closed publication: %+v", cached)
		}
	}
	if lifecycle != scm.PRStateOpen {
		observations := []scm.PRState{scm.PRStateOpen}
		if lifecycle == scm.PRStateMerged {
			observations = append(observations, scm.PRStateClosed)
		}
		for _, observed := range observations {
			t.Run("refuse_unpersistable_"+string(observed), func(t *testing.T) {
				env := sctx.Env
				sctx.Env = append(append([]string(nil), env...), "FAKE_CLI_PR_STATE="+string(observed))
				defer func() { sctx.Env = env }()
				for attempt := 0; attempt < 2; attempt++ {
					outcome, err := RecoverPublication(sctx)
					if err == nil || !strings.Contains(err.Error(), "persisted PR metadata does not match") || outcome != nil {
						t.Fatalf("retry %d reported success for unpersistable lifecycle %s: outcome=%+v error=%v", attempt, observed, outcome, err)
					}
					sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
					if err != nil || sctx.Run.PRState == nil || *sctx.Run.PRState != strings.ToLower(string(lifecycle)) || sctx.Run.PRURL == nil || *sctx.Run.PRURL != "https://github.com/test/repo/pull/99" || sctx.Run.Status != types.RunFailed || sctx.Run.Error == nil || *sctx.Run.Error != *originalRun.Error {
						t.Fatalf("refusal changed monotonic lifecycle or original verdict: %+v %v", sctx.Run, err)
					}
				}
			})
		}
	}
	if got := gitCmd(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"); got != originalRefs {
		t.Fatalf("recovery changed source/target refs: before=%s after=%s", originalRefs, got)
	}
	afterPins, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if err != nil || afterPins == nil || afterPins.JSON != originalPins.JSON || afterPins.Refusal == nil || *afterPins.Refusal != *originalPins.Refusal {
		t.Fatalf("retry changed original pins: %+v %v", afterPins, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "pr create") != 1 || strings.Contains(string(data), "pr edit") || strings.Contains(string(data), "\npush ") {
		t.Fatalf("retry mutated publication: %s", data)
	}
	afterPR, err := os.ReadFile(state)
	if err != nil || string(afterPR) != string(originalPR) {
		t.Fatalf("retry changed remote PR: %v", err)
	}
	afterBody, err := os.ReadFile(body)
	if err != nil || string(afterBody) != string(originalBody) {
		t.Fatalf("retry changed remote content: %v", err)
	}
	_, _, afterEvidence, err := publicationEvidence(sctx)
	if err != nil || afterEvidence != evidence {
		t.Fatalf("retry changed validation evidence: %v", err)
	}
}

func TestPublicationRecoveryExistingPRPersistenceFailureRefusesSuccess(t *testing.T) {
	for _, column := range []string{"pr_state", "pr_url"} {
		t.Run(column, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			database, err := db.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunErrorStatus(run.ID, "original publication refusal", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			// Reject exactly one lifecycle/URL write, not earlier eligibility reads.
			conn, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Exec("CREATE TRIGGER refuse_write BEFORE UPDATE OF " + column + " ON runs BEGIN SELECT RAISE(FAIL, 'write refused'); END"); err != nil {
				t.Fatal(err)
			}
			sctx := &pipeline.StepContext{Ctx: context.Background(), DB: database, Run: run}
			pr := &scm.PR{URL: "https://github.com/test/repo/pull/99", HeadSHA: "head", BaseBranch: "main"}
			step := &PRStep{recovery: &publicationPins{HeadSHA: "head", BaseBranch: "main"}}
			outcome, err := step.recoveredExistingPR(sctx, &recordingRetargetHost{state: scm.PRStateClosed}, pr)
			if err == nil || !strings.Contains(err.Error(), "write refused") || outcome != nil {
				t.Fatalf("persistence failure reported success: outcome=%+v error=%v", outcome, err)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil || stored.PRURL != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "original publication refusal" {
				t.Fatalf("persistence failure changed original verdict or URL: %+v %v", stored, err)
			}
			if column == "pr_state" && (stored.PRState == nil || *stored.PRState != "none" || stored.PRStateObservedAt != nil) {
				t.Fatalf("failed state write manufactured lifecycle: %v", stored.PRState)
			}
			if column == "pr_url" && (stored.PRState == nil || *stored.PRState != "closed") {
				t.Fatalf("failed URL write lost actual lifecycle: %v", stored.PRState)
			}
		})
	}
}

func TestPublicationRecoveryExistingPRReadbackErrorRefusesSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(run.ID, "original publication refusal", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TRIGGER fail_readback AFTER UPDATE OF pr_url ON runs
		WHEN OLD.pr_state = 'closed' AND NEW.pr_url = 'https://github.com/test/repo/pull/99'
		BEGIN UPDATE runs SET created_at = 'injected post-write readback error' WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil || run == nil || run.PRURL != nil || run.PRState == nil || *run.PRState != "none" {
		t.Fatalf("pre-write read failed or metadata already persisted: %+v %v", run, err)
	}
	sctx := &pipeline.StepContext{Ctx: context.Background(), DB: database, Run: run}
	pr := &scm.PR{URL: "https://github.com/test/repo/pull/99", HeadSHA: "head", BaseBranch: "main"}
	step := &PRStep{recovery: &publicationPins{HeadSHA: "head", BaseBranch: "main"}}
	outcome, readErr := step.recoveredExistingPR(sctx, &recordingRetargetHost{state: scm.PRStateClosed}, pr)
	var state, url, createdAt, status, originalError string
	var observedAt int64
	if err := conn.QueryRow(`SELECT pr_state, pr_url, created_at, pr_state_observed_at, status, error FROM runs WHERE id = ?`, run.ID).Scan(&state, &url, &createdAt, &observedAt, &status, &originalError); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || url != pr.URL || observedAt == 0 || createdAt != "injected post-write readback error" {
		t.Fatalf("fault did not follow both successful writes: state=%q URL=%q observed=%d created=%q", state, url, observedAt, createdAt)
	}
	if status != string(types.RunFailed) || originalError != "original publication refusal" {
		t.Fatalf("readback failure changed original verdict: status=%q error=%q", status, originalError)
	}
	if readErr == nil || !strings.Contains(readErr.Error(), "verify recovered PR metadata") || !strings.Contains(readErr.Error(), "injected post-write readback error") || outcome != nil {
		t.Fatalf("post-write readback failure reported success or lost cause: outcome=%+v error=%v", outcome, readErr)
	}
}

func TestPublicationRecoveryRiskPreservesAttributedScopeWithoutPrivateInstructions(t *testing.T) {
	findings := `{"findings":[],"risk_level":"medium","risk_rationale":"stale risk prose","resolved_by_fix":[{"id":"r1","description":"Restore records","severity":"error","action":"auto-fix"}],"unresolved_notes":[{"id":"n1","description":"Keep the limitation visible","severity":"warning","action":"ask-user"}]}`
	instructions := `{"findings":[{"id":"r1","description":"Original concern","severity":"error","action":"auto-fix","user_instructions":"PRIVATE DIRECTIVE: do not publish these words"}]}`
	ids, source := `["r1"]`, db.RoundSelectionSourceUser
	row := &db.StepResult{ID: "review", StepName: types.StepReview, FindingsJSON: &findings}
	round := &db.StepRound{Round: 2, UserFindingsJSON: &instructions, SelectedFindingIDs: &ids, SelectionSource: &source}
	prior := &db.BranchDecisionRound{RunID: "earlier-run", StepName: types.StepReview, Round: round}
	text := recoveryRiskLine([]*db.StepResult{row}, map[string][]*db.StepRound{"review": {round}}, prior)
	for _, want := range []string{"Medium", "Fixed without rereview", "not independently revalidated", "Unresolved review note", "Keep the limitation visible", "earlier run (historical decision, not certification of the current head)", "original concern is not certified as repaired", "instructions private"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in recovery risk text:\n%s", want, text)
		}
	}
	if strings.Contains(text, "PRIVATE DIRECTIVE") || strings.Contains(text, "stale risk prose") {
		t.Fatalf("private instructions or stale rationale leaked:\n%s", text)
	}
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

func TestPublicationRecoveryRenamedForkSamePRAndRefusalControls(t *testing.T) {
	for _, mode := range []string{"open", "closed", "merged", "equal-name", "foreign-source", "wrong-branch", "wrong-head", "wrong-pr-url", "ambiguous", "missing-persisted"} {
		t.Run(mode, func(t *testing.T) {
			source := "contributor/renamed-fork"
			if mode == "equal-name" {
				source = "contributor/repo"
			}
			sctx, _ := publicationRecoveryFixtureWithClassification(t, true, func(sctx *pipeline.StepContext) {
				sctx.Repo.ForkURL = "https://github.com/" + source
				// Keep every Git read on the fixture; no forge or real run is used.
				gitCmd(t, sctx.WorkDir, "config", "url."+sctx.WorkDir+".insteadOf", sctx.Repo.ForkURL)
			})
			url := "https://github.com/test/repo/pull/99"
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
				t.Fatal(err)
			}
			sctx.Run.PRURL = &url
			if mode == "wrong-pr-url" {
				otherURL := "https://github.com/test/repo/pull/98"
				if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, otherURL); err != nil {
					t.Fatal(err)
				}
				sctx.Run.PRURL = &otherURL
			}
			head, branch, actualSource := sctx.Run.HeadSHA, "feature", source
			switch mode {
			case "foreign-source":
				actualSource = "contributor/other-repo"
			case "wrong-branch":
				branch = "other"
			case "wrong-head":
				head = sctx.Run.BaseSHA
			}
			entry := map[string]any{"number": 99, "url": url, "state": strings.ToUpper(mode), "baseRefName": "main", "headRefOid": head, "headRefName": branch, "headRepository": map[string]string{"nameWithOwner": actualSource}, "headRepositoryOwner": map[string]string{"login": "contributor"}}
			entries := []map[string]any{entry}
			if mode == "ambiguous" {
				entries = append(entries, entry)
			} else if mode == "missing-persisted" {
				entries = nil
			}
			payload, _ := json.Marshal(entries)
			if entries == nil {
				payload = []byte("[]")
			}
			env, log := fakeGH(t, "")
			sctx.Env = append(env, "FAKE_CLI_PR_LIST_JSON="+string(payload))
			outcome, err := RecoverPublication(sctx)
			positive := mode == "open" || mode == "closed" || mode == "merged" || mode == "equal-name"
			if positive {
				if err != nil || outcome == nil || outcome.PRURL != url {
					t.Fatalf("exact source PR was not returned: outcome=%+v error=%v", outcome, err)
				}
			} else if err == nil {
				t.Fatalf("mismatched or missing pinned PR accepted: %+v", outcome)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "pr create") || strings.Contains(string(data), "pr edit") || strings.Contains(string(data), "\npush ") {
				t.Fatalf("idempotence/refusal created or changed a publication:\n%s", data)
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

func TestPublicationRecoveryRefusesTargetAdvanceDuringAgents(t *testing.T) {
	for _, phase := range []string{"draft", "validation"} {
		t.Run(phase, func(t *testing.T) {
			sctx, dir := publicationRecoveryFixture(t)
			env, log := fakeGH(t, "")
			sctx.Env = env
			ag := sctx.Agent.(*mockAgent)
			advanced := false
			advance := func() {
				gitCmd(t, dir, "update-ref", "refs/heads/main", sctx.Run.HeadSHA)
				advanced = true
			}
			if phase == "draft" {
				ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
					advance()
					return &agent.Result{Output: []byte(`{"title":"fix: parser","body":"## What Changed\n\n- Restore records"}`)}, nil
				}
			} else {
				validate := ag.validationFn
				ag.validationFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					advance()
					return validate(ctx, opts)
				}
			}
			if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "target branch/base") {
				t.Fatalf("moved target accepted: %v", err)
			}
			if !advanced {
				t.Fatal("agent did not advance the target")
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "pr create") || strings.Contains(string(data), "pr edit") {
				t.Fatalf("moved target mutated forge: %s", data)
			}
		})
	}
}

func TestPublicationRecoveryRefusesSSHAliasReroutingBeforeUploads(t *testing.T) {
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "ssh")
	hostFile := filepath.Join(t.TempDir(), "ssh-host")
	if err := os.WriteFile(hostFile, []byte("github.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "ssh-hostname")
	t.Setenv("FAKE_CLI_SSH_HOST_FILE", hostFile)
	sctx, _ := publicationRecoveryFixtureWithClassification(t, true, func(sctx *pipeline.StepContext) {
		sctx.Repo.UpstreamURL = "git@work:test/repo.git"
		sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub}
		sctx.Config.Test.Evidence.AttachMedia = true
	})
	if err := os.WriteFile(hostFile, []byte("mirror.ghe.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, log := fakeGH(t, "")
	sctx.Env = env
	pins, err := ValidatePublicationRecoveryRecords(sctx)
	if err != nil {
		t.Fatal(err)
	}
	uploader := &stubMediaUploader{t: t}
	step := &PRStep{recovery: pins, mediaUploader: uploader}
	if _, err := step.Execute(sctx); err == nil || !strings.Contains(err.Error(), "target branch/base") {
		t.Fatalf("rerouted alias accepted: %v", err)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("rerouted recovery uploaded evidence: %v", uploader.calls)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--hostname mirror.ghe.com") || strings.Contains(string(data), "pr create") || strings.Contains(string(data), "pr edit") {
		t.Fatalf("unexpected rerouted forge operations: %s", data)
	}
}

func TestPublicationRecoveryRefusesInsufficientIdentityPinsWithoutRetrofit(t *testing.T) {
	sctx, _ := publicationRecoveryFixture(t)
	original, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original.JSON), &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "ForgeIdentity")
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.PinPublicationIntent(run.ID, run.HeadSHA, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.RecordPublicationRefusal(run.ID, run.HeadSHA, *original.Refusal); err != nil {
		t.Fatal(err)
	}
	run.Status, run.Error = types.RunFailed, sctx.Run.Error
	sctx.Run = run
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "invalid publication intent") {
		t.Fatalf("insufficient pins accepted: %v", err)
	}
	after, err := sctx.DB.GetPublicationIntent(run.ID, run.HeadSHA)
	if err != nil || after.JSON != string(payload) {
		t.Fatalf("legacy pins retrofitted: %+v %v", after, err)
	}
}

func TestPublicationRecoveryGeneratedHistoryPrivacyProspectiveOnly(t *testing.T) {
	sctx, _ := publicationRecoveryClassifiedFixture(t, false, true)
	before, _, digest, err := publicationEvidence(sctx)
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if pin.Refusal == nil || !strings.Contains(*pin.Refusal, "generated-history privacy:") {
		t.Fatalf("not classified: %+v", pin)
	}
	env, log := fakeGH(t, "")
	sctx.Env = append(env, "FAKE_CLI_PUBLICATION_STATE="+filepath.Join(t.TempDir(), "forge-state.json"), "FAKE_CLI_PR_HEAD_SHA="+sctx.Run.HeadSHA)
	for i := 0; i < 2; i++ {
		outcome, err := RecoverPublication(sctx)
		if err != nil || outcome == nil || outcome.PRURL == "" {
			t.Fatalf("recovery=%+v %v", outcome, err)
		}
		sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	commands, _ := os.ReadFile(log)
	if strings.Contains(string(commands), "01ARZ3NDEKTSV4RRFFQ69G5FAV") || strings.Count(string(commands), "pr create") != 1 {
		t.Fatalf("privacy/idempotence failed: %s", commands)
	}
	after, _, afterDigest, _ := publicationEvidence(sctx)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	afterPin, _ := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if digest != afterDigest || string(a) != string(b) || pin.JSON != afterPin.JSON || *pin.Refusal != *afterPin.Refusal || sctx.Run.Status != types.RunFailed {
		t.Fatal("recovery changed immutable evidence or terminal refusal")
	}
}

func TestPublicationRecoveryRefusesSuccessorOwnership(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			sctx, _ := publicationRecoveryFixture(t)
			next, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				if err := sctx.DB.UpdateRunStatus(next.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ValidatePublicationRecoveryRecords(sctx); err == nil || (!strings.Contains(err.Error(), "active owner") && !strings.Contains(err.Error(), "current branch run")) {
				t.Fatalf("historical recovery allowed: %v", err)
			}
		})
	}
}

func TestPublicationRecoveryPrivacyDoesNotWaiveResidualLiteralPrivacy(t *testing.T) {
	sctx, _ := publicationRecoveryClassifiedFixture(t, false, true)
	ag := sctx.Agent.(*mockAgent)
	ag.runFn = func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"title":"fix: fixture","body":"## What Changed\n\nLiteral captured output URL: https://example.com/captured/01ARZ3NDEKTSV4RRFFQ69G5FAV"}`)}, nil
	}
	ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		var written prContent
		_, raw, _ := strings.Cut(opts.Prompt, "Run-written PR content JSON:\n")
		if json.NewDecoder(strings.NewReader(raw)).Decode(&written) != nil || !strings.Contains(written.Body, "01ARZ3NDEKTSV4RRFFQ69G5FAV") {
			t.Fatal("literal evidence was masked before exact guard")
		}
		output, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{"literal captured output exposes private identifier"}})
		return &agent.Result{Output: output}, nil
	}
	env, log := fakeGH(t, "")
	sctx.Env = env
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "literal captured output") {
		t.Fatalf("residual privacy bypassed: %v", err)
	}
	commands, _ := os.ReadFile(log)
	if strings.Contains(string(commands), "pr create") {
		t.Fatal("created PR despite residual privacy refusal")
	}
}

func TestPublicationRecoveryRefusesMovedPrivateHeadDespiteRetainedObject(t *testing.T) {
	sctx, dir := publicationRecoveryFixture(t)
	sctx.GateDir = dir
	gitCmd(t, dir, "checkout", "--detach", sctx.Run.HeadSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/feature", sctx.Run.BaseSHA)
	if _, err := RecoverPublication(sctx); err == nil || !strings.Contains(err.Error(), "private branch head") {
		t.Fatalf("retained object substituted for current head proof: %v", err)
	}
}
