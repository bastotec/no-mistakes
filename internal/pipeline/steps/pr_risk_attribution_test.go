package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A supported composite selection repairs concerns with ambiguous original IDs.
// The real executor must retain those originals; prose must not convert that
// undischarged ledger into newly confirmed defects. Later Test/docs commits
// likewise cannot prove that any historical claim is either repaired or present.
func TestOrdinaryRiskAttributesCompositeAndLaterStepRepairs(t *testing.T) {
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
	repo, err := database.InsertRepoWithID("risk-attribution", work, work, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commit := func(name, message string) (string, error) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(message+"\n"), 0600); err != nil {
			return "", err
		}
		if _, err := git.Run(ctx, work, "add", name); err != nil {
			return "", err
		}
		if _, err := git.Run(ctx, work, "commit", "-m", message); err != nil {
			return "", err
		}
		sha, err := git.Run(ctx, work, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		run.HeadSHA = strings.TrimSpace(sha)
		return run.HeadSHA, database.UpdateRunHeadSHA(run.ID, run.HeadSHA)
	}
	step := dispositionReview{execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if !sctx.Fixing {
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"review-1","severity":"warning","description":"[CONFIRMED] URL persistence is missing","action":"auto-fix"},{"id":"review-1","severity":"warning","description":"[CONFIRMED] policy resume is broken","action":"auto-fix"}],"risk_level":"medium","risk_rationale":"URL persistence is missing"}`, ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}
		if _, err := commit("selected.txt", "composite repair"); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{Findings: `{"findings":[{"id":"lifecycle","severity":"warning","description":"[CONFIRMED] lifecycle is incorrectly open","action":"auto-fix"}],"risk_level":"medium","risk_rationale":"lifecycle is incorrectly open"}`, FixSummary: "composite repair committed", ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	executor := pipeline.NewExecutor(database, p, &config.Config{Review: config.Review{MaxRounds: 2}}, nil, []pipeline.Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, work) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("executor did not stop")
		}
	}()
	for {
		rows, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 && rows[0].Status == types.StepStatusAwaitingApproval {
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
	added := []types.Finding{{ID: "composite-repair", Severity: "warning", Action: types.ActionAutoFix, Description: "Repair both original concerns"}}
	if err := executor.RespondWithOverrides(types.StepReview, types.ActionFix, nil, nil, added); err != nil {
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
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := rows[0]
	rounds, err := database.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || review.Status != types.StepStatusCompleted {
		t.Fatalf("unexpected review: %+v rounds=%d", review, len(rounds))
	}
	final, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.UnresolvedNotes) != 3 || len(final.ResolvedByFix) != 0 || final.RiskLevel != "medium" {
		t.Fatalf("ledger changed: %+v", final)
	}
	if !strings.Contains(final.RiskRationale, `carried warning review-1 (current validity not reverified): "[CONFIRMED] URL persistence is missing"`) || strings.Contains(final.Summary, "not fixed") {
		t.Errorf("historical selection ledger became current defect claims: %+v", final)
	}
	original := *review.FindingsJSON
	reviewed := run.HeadSHA
	byStep := map[string][]*db.StepRound{review.ID: rounds}
	_, currentRisk := BuildPipelineSummary(rows, byStep, reviewed)
	if !strings.Contains(currentRisk, "lifecycle is incorrectly open") || strings.Contains(currentRisk, "Historical Review assessment:") {
		t.Errorf("genuine current risk lost: %s", currentRisk)
	}
	// Record a later Test repair and its bounded evidence, then a docs-only commit.
	// These are deterministic persisted inputs, not live acceptance or a rereview.
	for _, phase := range []string{"test", "document"} {
		filename := "product.txt"
		if phase == "document" {
			filename = "README.md"
		}
		if _, err := commit(filename, phase+" follow-up"); err != nil {
			t.Fatal(err)
		}
		sr, err := database.InsertStepResult(run.ID, types.StepName(phase))
		if err != nil {
			t.Fatal(err)
		}
		payload := `{"findings":[],"summary":"follow-up committed"}`
		if phase == "test" {
			payload = fmt.Sprintf(`{"findings":[],"verdict":"inconclusive","tested_head_sha":%q,"testing_summary":"deterministic regressions only; no live consumer acceptance"}`, run.HeadSHA)
		}
		if err := database.SetStepFindings(sr.ID, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertStepRound(sr.ID, 1, "auto_fix", &payload, nil, 0); err != nil {
			t.Fatal(err)
		}
		rows, err = database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			rs, err := database.GetRoundsByStep(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			byStep[row.ID] = rs
		}
		md, risk := BuildPipelineSummary(rows, byStep, run.HeadSHA)
		body := appendGeneratedSectionsToCleanBody("## What Changed\n\nBounded repair.", risk, "", md)
		for _, want := range []string{"## Risk Assessment", "Medium (recorded Review rating)", "Historical Review assessment:", reviewed, "not certification of the current head", "neither a repair nor a continuing defect", fmt.Sprintf("%q", final.RiskRationale), "[CONFIRMED] URL persistence is missing", "[CONFIRMED] policy resume is broken", "[CONFIRMED] lifecycle is incorrectly open", "unresolved review note"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q in assembled ordinary body:\n%s", phase, want, body)
			}
		}
		reread, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range reread {
			if row.StepName == types.StepReview && (row.FindingsJSON == nil || !reflect.DeepEqual(*row.FindingsJSON, original)) {
				t.Fatal("rendering rewrote historical findings")
			}
		}
	}
}
