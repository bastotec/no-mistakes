package steps

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type dispositionReview struct {
	execute func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s dispositionReview) Name() types.StepName { return types.StepReview }
func (s dispositionReview) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.execute(ctx)
}

// Synthetic reviewer outputs drive real gate selection, repair commits, durable
// rounds, round-cap completion and public rendering. No forge/model is contacted.
func TestReviewDispositionLifecycleRendersOnlyCurrentRisk(t *testing.T) {
	work, base, head := setupGitRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID("disposition", work, work, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	step := dispositionReview{execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if !sctx.Fixing {
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"repair","severity":"error","description":"selected routing defect","action":"auto-fix"},{"id":"held","severity":"warning","description":"different surviving defect","action":"auto-fix"}],"summary":"selected routing defect remains","risk_level":"high","risk_rationale":"selected routing defect remains"}`}, nil
		}
		if err := os.WriteFile(filepath.Join(work, "repair.txt"), []byte("synthetic repair\n"), 0600); err != nil {
			return nil, err
		}
		if _, err := git.Run(sctx.Ctx, work, "add", "repair.txt"); err != nil {
			return nil, err
		}
		if _, err := git.Run(sctx.Ctx, work, "commit", "-m", "fix: synthetic selected repair"); err != nil {
			return nil, err
		}
		fixed, err := git.Run(sctx.Ctx, work, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		sctx.Run.HeadSHA = strings.TrimSpace(fixed)
		if err := database.UpdateRunHeadSHA(run.ID, sctx.Run.HeadSHA); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{Findings: `{"findings":[],"summary":"fresh review complete","risk_level":"low","risk_rationale":"no new actionable findings"}`, FixSummary: "changes applied", ReviewApprovedHeadSHA: sctx.Run.HeadSHA}, nil
	}}
	executor := pipeline.NewExecutor(database, p, &config.Config{Review: config.Review{MaxRounds: 2}}, nil, []pipeline.Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, work) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("executor did not exit")
		}
	}()
	for {
		results, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) > 0 && results[0].Status == types.StepStatusAwaitingApproval {
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("executor exited before gate: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := executor.Respond(types.StepReview, types.ActionFix, []string{"repair"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != types.StepStatusCompleted || len(rounds) != 2 || run.HeadSHA == head {
		t.Fatalf("unexpected lifecycle: %+v rounds=%d head=%s", results[0], len(rounds), run.HeadSHA)
	}
	final, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Items) != 0 || len(final.UnresolvedNotes) != 1 || final.UnresolvedNotes[0].ID != "held" {
		t.Fatalf("lost real warning: %+v", final)
	}
	md, risk := BuildPipelineSummary(results, map[string][]*db.StepRound{results[0].ID: rounds}, run.HeadSHA)
	if strings.Contains(risk, "selected routing defect") || !strings.Contains(risk, "different surviving defect") || !strings.Contains(risk, "Medium") || !strings.Contains(md, "unresolved review note") || !strings.Contains(md, "different surviving defect") {
		t.Fatalf("public disposition is inaccurate:\n%s\nRisk: %s", md, risk)
	}
	t.Logf("Synthetic publication evidence at %s:\n%s\nRisk: %s", run.HeadSHA, md, risk)
}

func TestReviewBoundedApprovalLifecycleReachesPublicationGuard(t *testing.T) {
	work, base, head := setupGitRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID("disposition", work, work, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	step := dispositionReview{execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if !sctx.Fixing {
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"repair","severity":"error","description":"original product repair is absent","action":"auto-fix"}],"summary":"selected routing defect remains","risk_level":"high","risk_rationale":"selected routing defect remains"}`}, nil
		}
		// The accepted intermediate requires diagnosis only: deliberately
		// leave the product tree unchanged, as in a supported no-op repair.
		if !strings.Contains(sctx.PreviousFindings, "Accept diagnostic intermediate only") {
			t.Fatal("fixer lost bounded instructions")
		}
		return &pipeline.StepOutcome{Findings: `{"findings":[],"summary":"diagnostic scope assessed","risk_level":"medium","risk_rationale":"independent residual risk"}`, FixSummary: "diagnostic assessment only; no source change", ReviewApprovedHeadSHA: sctx.Run.HeadSHA}, nil
	}}
	executor := pipeline.NewExecutor(database, p, &config.Config{Review: config.Review{MaxRounds: 2}}, nil, []pipeline.Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, work) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("executor did not exit")
		}
	}()
	for {
		results, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) > 0 && results[0].Status == types.StepStatusAwaitingApproval {
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("executor exited before gate: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := executor.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"repair"}, map[string]string{"repair": "Accept diagnostic intermediate only; original product repair remains out of scope and unresolved."}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != types.StepStatusCompleted || len(rounds) != 2 || run.HeadSHA != head {
		t.Fatalf("unexpected lifecycle: %+v rounds=%d head=%s", results[0], len(rounds), run.HeadSHA)
	}
	final, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Items) != 0 || len(final.UnresolvedNotes) != 0 {
		t.Fatalf("original defect falsely revived: %+v", final)
	}
	md, risk := BuildPipelineSummary(results, map[string][]*db.StepRound{results[0].ID: rounds}, run.HeadSHA)
	if !strings.Contains(risk, "diagnostic intermediate") || !strings.Contains(risk, "independent residual risk") || strings.Contains(md, "Re-checked - no issues remain") || !strings.Contains(md, "original concern is not certified as repaired") {
		t.Fatalf("bounded approval misrepresented:\n%s\nRisk: %s", md, risk)
	}
	// Reopen SQLite: publication must consume durable decisions, not an
	// in-memory response or the reviewer's own prose claiming approval.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, approved := range []bool{true, false} {
		checker := &mockAgent{name: "controlled-checker", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// The writer must see the same attributed evidence as the checker.
			evidence := strings.Contains(opts.Prompt, `user_instructions`) && strings.Contains(opts.Prompt, "Accept diagnostic intermediate only")
			if evidence != approved {
				t.Fatalf("writer decision context: evidence=%v approved=%v", evidence, approved)
			}
			return &agent.Result{Output: json.RawMessage(`{"title":"fix: diagnostic intermediate","body":"## What Changed\n\nApproved diagnostic intermediate, not the original repair.","unsupported_rules":[]}`)}, nil
		}, validationFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			digest, err := validationDigestFromPrompt(opts.Prompt)
			if err != nil {
				return nil, err
			}
			// A deterministic stand-in for the real semantic checker: only
			// attributed recorded instructions support this bounded claim.
			evidence := strings.Contains(opts.Prompt, "user chose to fix:") && strings.Contains(opts.Prompt, `user_instructions`) && strings.Contains(opts.Prompt, "Accept diagnostic intermediate only")
			issues := []string{}
			if !evidence {
				issues = append(issues, "approval claim lacks recorded evidence")
			}
			payload, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: issues})
			return &agent.Result{Output: payload}, nil
		}}
		sctx := newTestContext(t, checker, work, base, run.HeadSHA, config.Commands{})
		if approved {
			sctx.DB, sctx.Run, sctx.Repo = reopened, run, repo
		}
		_, err := (&PRStep{}).buildPRContentWithPolicy(sctx, "feature", "main", base, base, scm.ProviderGitHub, githubPullRequestBodyHardLimitChars)
		if (err == nil) != approved {
			t.Fatalf("approved=%v: publication guard: %v", approved, err)
		}
	}

	t.Logf("Synthetic publication evidence at %s:\n%s\nRisk: %s", run.HeadSHA, md, risk)
}

func TestReviewScopeRequiresRecordedSelectedUserInstructions(t *testing.T) {
	selected := `["repair"]`
	user := db.RoundSelectionSourceUser
	auto := db.RoundSelectionSourceAutoFix
	declined := db.RoundSelectionSourceUserDeclined
	payload := `{"findings":[{"id":"repair","severity":"error","description":"original remedy","user_instructions":"diagnostic only"}]}`
	for _, tc := range []struct {
		name   string
		source *string
		ids    *string
		want   int
	}{
		{"selected user scope", &user, &selected, 1},
		{"automatic selection is not approval", &auto, &selected, 0},
		{"declined is not selected approval", &declined, &selected, 0},
		{"no decision", nil, &selected, 0},
		{"missing selection", &user, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewScopeDecisions([]*db.StepRound{{Round: 1, SelectionSource: tc.source, SelectedFindingIDs: tc.ids, UserFindingsJSON: &payload}})
			if len(got) != tc.want {
				t.Fatalf("scope decisions = %v", got)
			}
		})
	}
}
