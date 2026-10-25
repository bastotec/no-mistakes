package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runconfine"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Start a later real review repair with both index and working-tree progress.
// It must hand that progress to the fixer, not reset to the recorded head.
func TestReviewFixContinuesExistingUncommittedWork(t *testing.T) {
	for _, confined := range []bool{false, true} {
		t.Run(fmt.Sprintf("confined=%v", confined), func(t *testing.T) {
			testReviewFixContinuesExistingUncommittedWork(t, confined)
		})
	}
}

func testReviewFixContinuesExistingUncommittedWork(t *testing.T, confined bool) {
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
			if !strings.Contains(opts.Prompt, "restored before you started") || !strings.Contains(opts.Prompt, "staged-repair.txt") || !strings.Contains(opts.Prompt, "selected findings") {
				t.Fatal("missing continuation instructions")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"finish prior repair"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	// The executor restores captured work before a fix round and names it.
	sctx.RestoredSurvivedWork = &types.SurvivedWork{Files: []string{"A  staged-repair.txt", "?? unstaged-repair.txt"}}
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","description":"finish repair","severity":"error","action":"auto-fix"}]}`
	if confined {
		// Exercise the actual host request, JSON boundary and worker rebuild,
		// without namespaces, services or a live provider.
		input := newIsolatedRunInput(sctx, &ReviewStep{}, "synthetic", runconfine.DefaultPolicy(), "synthetic")
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var request isolatedRunInput
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		worker := request.stepContext(sctx.DB, ag, json.NewDecoder(strings.NewReader(`""`)), func(isolatedStepEvent) error { return nil })
		if !reflect.DeepEqual(worker.RestoredSurvivedWork, sctx.RestoredSurvivedWork) {
			t.Fatalf("restored repair metadata lost: got %+v, want %+v", worker.RestoredSurvivedWork, sctx.RestoredSurvivedWork)
		}
		// Head/index transport has independent protocol coverage. This test
		// observes the real repair prompt and commit in the synthetic worktree.
		worker.RecordIsolatedHead = nil
		sctx = worker
	}
	if _, _, err := resolveCIRepairBases(context.Background(), sctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
	if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("successful boundary did not commit progress: %q", got)
	}
	for _, name := range []string{"staged-repair.txt", "unstaged-repair.txt"} {
		if data := gitCmd(t, dir, "show", "HEAD:"+name); data != "prior partial repair" {
			t.Fatalf("progress not committed: %s", data)
		}
	}
}
