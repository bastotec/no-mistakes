package steps

import (
	"context"
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
