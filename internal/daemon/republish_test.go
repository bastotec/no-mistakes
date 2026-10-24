package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// terminalRunWithFailedPR inserts a terminal run whose PR step row records a
// failure, mirroring what an exhausted publication refusal leaves behind.
func terminalRunWithFailedPR(t *testing.T, d *db.DB, repoID, branch, head string) *db.Run {
	t.Helper()
	run, err := d.InsertRun(repoID, branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		sr, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepPR {
			if err := d.FailStep(sr.ID, "refusing non-compliant PR publication", 10); err != nil {
				t.Fatal(err)
			}
		} else if err := d.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 5, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunErrorStatus(run.ID, "step pr failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRepublishPublication_RefusesIneligibleRuns(t *testing.T) {
	p, d := startTestDaemon(t)
	repo, head := setupTestGitRepo(t, p, d, "republish-refusals")

	mkParams := func(run *db.Run) ipc.RepublishPublicationParams {
		return ipc.RepublishPublicationParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: head}
	}
	// A non-terminal run: insert, leave pending, no step rows.
	pending, err := d.InsertRun(repo.ID, "pending-branch", head, head)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		params func() ipc.RepublishPublicationParams
		want   string
	}{
		{"unknown-run", func() ipc.RepublishPublicationParams {
			return ipc.RepublishPublicationParams{RepoID: repo.ID, RunID: "01M5ZZZZZZZZZZZZZZZZZZZZZZ", HeadSHA: head}
		}, "does not belong"},
		{"non-terminal", func() ipc.RepublishPublicationParams { return mkParams(pending) }, "not terminal"},
		{"head-mismatch", func() ipc.RepublishPublicationParams {
			run := terminalRunWithFailedPR(t, d, repo.ID, "mismatch-branch", head)
			params := mkParams(run)
			params.HeadSHA = strings.Repeat("a", 40)
			return params
		}, "recorded head"},
		{"no-failed-pr-step", func() ipc.RepublishPublicationParams {
			run, err := d.InsertRun(repo.ID, "green-branch", head, head)
			if err != nil {
				t.Fatal(err)
			}
			sr, err := d.InsertStepResult(run.ID, types.StepPR)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 5, ""); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "no failed PR step"},
		{"missing-pr-row", func() ipc.RepublishPublicationParams {
			run, err := d.InsertRun(repo.ID, "no-pr-row-branch", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "no failed PR step"},
		{"absent-head", func() ipc.RepublishPublicationParams {
			// A head the gate does not have: the run recorded an object that
			// never reached the bare repo, so the recorded head itself is absent.
			run := terminalRunWithFailedPR(t, d, repo.ID, "absent-head-branch", strings.Repeat("b", 40))
			params := mkParams(run)
			params.HeadSHA = strings.Repeat("b", 40)
			return params
		}, "absent from the gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewRunManager(d, p, nil)
			_, err := mgr.HandleRepublishPublication(t.Context(), tc.params())
			if err == nil || !strings.Contains(err.Error(), firstWant(tc.want)) {
				t.Fatalf("want refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

func firstWant(want string) string {
	if i := strings.Index(want, ","); i > 0 {
		return want[:i]
	}
	return want
}

// learnedPlaceholderForDaemon mirrors the steps package's learned-placeholder
// token; the assertion only needs its literal text.
func learnedPlaceholderForDaemon() string { return "[redacted]" }

// writeRepublishMockClaude writes a mock claude that answers the two PR-step
// invocations: the draft turn returns a valid prContent draft (with a private
// literal the sanitizer must strip on retry), and the validation turn returns
// a schema-valid verdict bound to the digest carried in its prompt.
func writeRepublishMockClaude(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
prompt=$(cat)
refusal='What Changed exposes the private identifier ` + "`gate-token-7f3k`" + `.'
case "$prompt" in
  *"Draft a pull request title"*)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"title":"fix: repair widget","body":"## What Changed\n\n- repair widget behind gate-token-7f3k","unsupported_rules":[]}}'
    ;;
  *"Validate the pull-request title"*)
    digest=$(printf '%s' "$prompt" | sed -n 's/.*content_sha256: \([0-9a-f]\{64\}\).*/\1/p' | head -n1)
    if [ -z "$digest" ]; then
      exit 1
    fi
    marker="` + filepath.Join(dir, "validated") + `"
    if [ ! -e "$marker" ]; then
      touch "$marker"
      printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"content_sha256\":\"$digest\",\"english\":true,\"format_compliant\":true,\"issues\":[\"$refusal\"]}}"
    else
      # A refusal after a redaction must not recur: the validator feedback
      # section legitimately quotes the refused issue, so the guard inspects
      # only the run-written content JSON, which carries the sanitized draft.
      if printf '%s' "$prompt" | grep -A2 "Run-written PR content JSON" | grep -q "gate-token-7f3k"; then
        exit 1
      fi
      printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"content_sha256\":\"$digest\",\"english\":true,\"format_compliant\":true,\"issues\":[]}}"
    fi
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRepublishPublication_ContinuesTerminalRunThroughSanitizedRetry proves
// the capability this task exists for: a TERMINAL run whose PR step failed on
// a publication refusal is republished by the continuation, with the ordinary
// sanitize-and-retry loop redacting the refused literal before the forge
// write, and with the recorded validation evidence rendered as recorded (no
// step re-run). It also proves no live executor is required.
func TestRepublishPublication_ContinuesTerminalRunThroughSanitizedRetry(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	t.Setenv("FAKE_CLI_PR_BODY_FILE", bodyFile)

	root, err := os.MkdirTemp("", "republish-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	mockDir, err := os.MkdirTemp("", "republish-mock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(mockDir) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeRepublishMockClaude(t, mockDir)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\npush_quiet_window: \"0s\"\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	repo, head := setupTestGitRepo(t, p, d, "republish-positive")
	mgr := NewRunManager(d, p, nil)
	run := terminalRunWithFailedPR(t, d, repo.ID, "feature", head)

	result, err := mgr.HandleRepublishPublication(t.Context(), ipc.RepublishPublicationParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: head})
	if err != nil {
		t.Fatalf("HandleRepublishPublication = %v, want a sanitized retry to publish", err)
	}
	if result.PRURL == "" || result.HeadSHA != head {
		t.Fatalf("result = %+v", result)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "gate-token-7f3k") {
		t.Fatalf("published PR body still carries the refused literal:\n%s", body)
	}
	if !strings.Contains(string(body), "repair widget behind "+learnedPlaceholderForDaemon()) {
		t.Fatalf("published PR body lost the sanitized narrative:\n%s", body)
	}
	// The PR URL lands on the terminal run record, exactly like the ordinary
	// PR step's durable write, and nothing else about the run changes.
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PRURL == nil || *stored.PRURL != result.PRURL {
		t.Fatalf("run PR URL not recorded: %+v", stored)
	}
	if stored.Status != types.RunFailed {
		t.Fatalf("continuation mutated the terminal run status: %s", stored.Status)
	}
	// The throwaway worktree is gone.
	if _, err := os.Stat(filepath.Join(p.WorktreeDir(repo.ID, run.ID), "republish")); !os.IsNotExist(err) {
		t.Fatalf("republish worktree survived: %v", err)
	}
}
