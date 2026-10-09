package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Start a later real review repair with both index and working-tree progress.
// It must hand that progress to the fixer, not reset to the recorded head.
func TestReviewFixContinuesExistingUncommittedWork(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	staged := filepath.Join(dir, "staged-repair.txt")
	unstaged := filepath.Join(dir, "unstaged-repair.txt")
	for _, file := range []string{staged, unstaged} {
		if err := os.WriteFile(file, []byte("prior partial repair"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "staged-repair.txt")
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			for _, file := range []string{staged, unstaged} {
				if data, err := os.ReadFile(file); err != nil || string(data) != "prior partial repair" {
					t.Fatalf("prior progress lost: %s %v", data, err)
				}
			}
			if !strings.Contains(opts.Prompt, "Existing uncommitted repair work") || !strings.Contains(opts.Prompt, "selected findings") {
				t.Fatal("missing continuation instructions")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"finish prior repair"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","description":"finish repair","severity":"error","action":"auto-fix"}]}`
	if _, _, err := resolveCIRepairBases(context.Background(), sctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
	if work := pipeline.InspectSurvivedWork(dir); work != nil {
		t.Fatalf("successful boundary did not commit progress: %+v", work)
	}
	for _, name := range []string{"staged-repair.txt", "unstaged-repair.txt"} {
		if data := gitCmd(t, dir, "show", "HEAD:"+name); data != "prior partial repair" {
			t.Fatalf("progress not committed: %s", data)
		}
	}
}
