package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise real steps through the public adapter factory, not a mock Agent.
func TestDeckReviewFixAndTestExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, duty := range []string{"review", "review-fix", "test-evidence"} {
		t.Run(duty, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", head)
			fixture := t.TempDir()
			bin := filepath.Join(fixture, "deck")
			event := func(output string) string {
				b, err := json.Marshal(map[string]string{"type": "run_finished", "output": output})
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			response := cleanReviewJSON
			if duty == "test-evidence" {
				response = passingScenarioFindingsJSON
			}
			script := "#!/bin/sh\ncat >/dev/null\n"
			if duty == "review-fix" {
				script += fmt.Sprintf("if [ ! -f '%s/fixed' ]; then\n touch '%s/fixed'\n printf 'repaired\\n' > deck-fix.txt\n printf '%%s\\n' '%s'\n exit 0\nfi\n", fixture, fixture, event(`{"summary":"repair the reported defect"}`))
			}
			script += fmt.Sprintf("printf '%%s\\n' '%s'\n", strings.ReplaceAll(event(response), "'", "'\\''"))
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ag, err := agent.New(types.AgentDeck, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ag.Close()
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			if duty == "review-fix" {
				sctx.Fixing = true
				sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"error","action":"auto-fix","description":"repair the defect"}]}`
			}
			if duty == "test-evidence" {
				out, err := (&TestStep{}).Execute(sctx)
				if err != nil || out.NeedsApproval {
					t.Fatalf("Test outcome=%+v err=%v", out, err)
				}
			} else {
				out, err := (&ReviewStep{}).Execute(sctx)
				if err != nil || out.NeedsApproval {
					t.Fatalf("Review outcome=%+v err=%v", out, err)
				}
			}
			if duty == "review-fix" {
				if _, err := os.Stat(filepath.Join(dir, "deck-fix.txt")); err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
					t.Fatalf("fix was not committed: %s", got)
				}
			}
		})
	}
}
