package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func survivedWorkRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}, {"commit", "--allow-empty", "-m", "base"}} {
		if _, err := git.Run(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestInspectSurvivedWork(t *testing.T) {
	dir := survivedWorkRepo(t)
	if work := InspectSurvivedWork(dir); work != nil {
		t.Fatalf("clean checkout: %+v", work)
	}
	for _, name := range []string{"staged.txt", "untracked.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial repair"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git.Run(context.Background(), dir, "add", "staged.txt"); err != nil {
		t.Fatal(err)
	}
	work := InspectSurvivedWork(dir)
	if work == nil || work.Worktree != dir || len(work.Files) != 2 {
		t.Fatalf("snapshot: %+v", work)
	}
	raw, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: work})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(normalizeFindingsJSON(raw, "test"))
	if err != nil || parsed.SurvivedWork == nil || parsed.SurvivedWork.Worktree != dir {
		t.Fatalf("metadata lost: %s %v", raw, err)
	}
	if prompt := SurvivedWorkPrompt(dir); !strings.Contains(prompt, "Do not reset or clean") || !strings.Contains(prompt, "selected findings") {
		t.Fatal(prompt)
	}
	if missing := InspectSurvivedWork(filepath.Join(dir, "missing")); missing == nil || missing.InspectionError == "" {
		t.Fatalf("unreadable status treated as clean: %+v", missing)
	}
}

func TestExecutor_TimedOutRepairPersistsSurvivedWork(t *testing.T) {
	for _, timeout := range []error{ErrAgentTimeout, ErrReviewAgentTimeout} {
		t.Run(timeout.Error(), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			dir := survivedWorkRepo(t)
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if !sctx.Fixing {
					return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"},{"id":"deferred","severity":"warning","description":"wait","action":"ask-user"}]}`}, nil
				}
				if err := os.WriteFile(filepath.Join(dir, "progress.txt"), []byte("survived"), 0600); err != nil {
					t.Fatal(err)
				}
				return nil, timeout
			}}
			exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
			if err := exec.Execute(context.Background(), run, repo, dir); err == nil {
				t.Fatal("timeout passed")
			}
			sr := stepResultByName(t, database, run.ID, types.StepTest)
			if sr.FindingsJSON == nil {
				t.Fatal("missing durable findings")
			}
			findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
			if err != nil || findings.SurvivedWork == nil || findings.SurvivedWork.Worktree != dir || len(findings.SurvivedWork.Files) != 1 {
				t.Fatalf("survival missing: %+v %v", findings, err)
			}
			if len(findings.Items) != 2 || findings.Items[0].ID != "selected" || findings.Items[1].ID != "deferred" {
				t.Fatalf("selected and deferred findings not preserved: %+v", findings.Items)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "progress.txt")); err != nil || string(data) != "survived" {
				t.Fatalf("repair erased: %s %v", data, err)
			}
		})
	}
}

func TestExecutor_RetainsSurvivedWorkAcrossLaterOutcome(t *testing.T) {
	database, p, run, _ := setupTest(t)
	dir := survivedWorkRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("survived"), 0600); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: InspectSurvivedWork(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(step.ID, prior); err != nil {
		t.Fatal(err)
	}
	next := `{"findings":[{"id":"later","severity":"error","description":"still red","action":"auto-fix"}]}`
	exec := NewExecutor(database, p, &config.Config{}, nil, nil, nil)
	retained, err := types.ParseFindingsJSON(exec.retainSurvivedWork(step.ID, dir, next))
	if err != nil || retained.SurvivedWork == nil || len(retained.Items) != 1 || retained.Items[0].ID != "later" {
		t.Fatalf("later outcome lost survival metadata: %+v %v", retained, err)
	}
}
