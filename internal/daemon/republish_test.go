package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// terminalRunWithFailedPR inserts a terminal run whose PR step row records a
// failure, mirroring what an exhausted publication refusal leaves behind. The
// run's PR target is pinned to main, as a launch that resolved its target
// would have recorded.
func terminalRunWithFailedPR(t *testing.T, d *db.DB, repoID, branch, head string) *db.Run {
	t.Helper()
	run, err := d.InsertRunWithIntent(repoID, branch, head, head, nil, "main")
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
			run, err := d.InsertRunWithIntent(repo.ID, "green-branch", head, head, nil, "main")
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
			run, err := d.InsertRunWithIntent(repo.ID, "no-pr-row-branch", head, head, nil, "main")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "no failed PR step"},
		{"preserve-history", func() ipc.RepublishPublicationParams {
			run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "protected-branch", head, head, nil, "", "", "", "main", head)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "preserve-history run"},
		{"active-branch", func() ipc.RepublishPublicationParams {
			run := terminalRunWithFailedPR(t, d, repo.ID, "owned-branch", head)
			if _, err := d.InsertRun(repo.ID, "owned-branch", head, head); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "has active run"},
		{"absent-head", func() ipc.RepublishPublicationParams {
			// A head the gate does not have: the run recorded an object that
			// never reached the bare repo, so the recorded head itself is absent.
			run := terminalRunWithFailedPR(t, d, repo.ID, "absent-head-branch", strings.Repeat("b", 40))
			params := mkParams(run)
			params.HeadSHA = strings.Repeat("b", 40)
			return params
		}, "absent from the gate"},
		{"no-recorded-target", func() ipc.RepublishPublicationParams {
			// R7: a run that never pinned a PR target cannot prove where it
			// published, and re-resolving from live trusted config would let a
			// later config change redirect an already-validated head.
			run, err := d.InsertRun(repo.ID, "no-target-branch", head, head)
			if err != nil {
				t.Fatal(err)
			}
			sr, err := d.InsertStepResult(run.ID, types.StepPR)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.FailStep(sr.ID, "refusing non-compliant PR publication", 10); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunErrorStatus(run.ID, "step pr failed", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return mkParams(run)
		}, "recorded no PR target branch"},
		{"moved-remote-head", func() ipc.RepublishPublicationParams {
			// R1: a newer run published a different head on the same branch, so
			// this run's attestation must not overwrite the newer PR body. The
			// repo's remote must be ls-remote reachable, so register a repo
			// whose upstream is a real bare remote and publish a newer head on
			// the branch than the run's recorded head.
			bare := filepath.Join(t.TempDir(), "remote.git")
			gitCmd(t, "", "init", "--bare", bare)
			work := filepath.Join(t.TempDir(), "work")
			gitCmd(t, "", "clone", repo.WorkingPath, work)
			rmt, err := d.InsertRepoWithIDAndFork("republish-moved", work, bare, "", "main")
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, work, "push", rmt.UpstreamURL, "HEAD:refs/heads/moved-branch", "HEAD:refs/heads/main")
			gitCmd(t, work, "commit", "--allow-empty", "-m", "newer head")
			gitCmd(t, work, "push", "--force", rmt.UpstreamURL, "HEAD:refs/heads/moved-branch")
			gitCmd(t, "", "init", "--bare", p.RepoDir(rmt.ID))
			gitCmd(t, p.RepoDir(rmt.ID), "fetch", rmt.UpstreamURL, "refs/heads/moved-branch:refs/heads/moved-branch", "refs/heads/main:refs/heads/main")
			run := terminalRunWithFailedPR(t, d, rmt.ID, "moved-branch", head)
			return ipc.RepublishPublicationParams{RepoID: rmt.ID, RunID: run.ID, HeadSHA: head}
		}, "newer publication owns this branch"},
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

// gitAnyOutput runs git and returns its raw stdout (possibly empty) without
// the trim-one-newline assumption of gitOutput, for commands that may legally
// print nothing.
func gitAnyOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(out)
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

// republishReachableFixture registers a repo whose source branch is verifiable
// over a real remote: R1's guard ls-remotes the live source branch, so the
// fixture pushes the head to a real bare remote (recorded as the fork push
// target) and gives the gate its own copy plus an origin pointing there. The
// upstream URL keeps a github identity so provider resolution behaves as in
// production.
func republishReachableFixture(t *testing.T, p *paths.Paths, d *db.DB, id string, branches ...string) (*db.Repo, string) {
	t.Helper()
	base, head := setupTestGitRepo(t, p, d, id+"-base")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCmd(t, "", "init", "--bare", remote)
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", "clone", base.WorkingPath, work)
	specs := make([]string, 0, len(branches)+1)
	specs = append(specs, "HEAD:refs/heads/main")
	for _, b := range branches {
		if b == "main" {
			continue
		}
		specs = append(specs, "HEAD:refs/heads/"+b)
	}
	gitCmd(t, work, append([]string{"push", remote}, specs...)...)
	repo, err := d.InsertRepoWithIDAndFork(id, work, base.UpstreamURL, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, "", "init", "--bare", p.RepoDir(repo.ID))
	fetchSpecs := make([]string, 0, len(branches)+1)
	fetchSpecs = append(fetchSpecs, "refs/heads/main:refs/heads/main")
	for _, b := range branches {
		fetchSpecs = append(fetchSpecs, "refs/heads/"+b+":refs/heads/"+b)
	}
	gitCmd(t, p.RepoDir(repo.ID), append([]string{"fetch", remote}, fetchSpecs...)...)
	// The gate's origin points at the real remote so a run worktree's
	// trusted-config fetch (which resolves origin's URL) can reach the
	// default branch.
	gitCmd(t, p.RepoDir(repo.ID), "remote", "add", "origin", remote)
	return repo, head
}

// TestRepublishPublication_ContinuesTerminalRunThroughSanitizedRetry proves
// the capability this task exists for: a TERMINAL run whose PR step failed on
// a publication refusal is republished by the continuation, with the ordinary
// sanitize-and-retry loop redacting the refused literal before the forge
// write, and with the recorded validation evidence rendered as recorded (no
// step re-run). It also proves no live executor is required.
func TestRepublishPublication_ContinuesTerminalRunThroughSanitizedRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock Claude requires a POSIX shell")
	}
	bin := t.TempDir()
	name := "gh"
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

	repo, head := republishReachableFixture(t, p, d, "republish-positive", "feature")
	rmt := repo
	run := terminalRunWithFailedPR(t, d, rmt.ID, "feature", head)
	rowsBefore, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A publication continuation must not upgrade a failed validation or
	// synthesize the Intent/Rebase rows this fixture never recorded.
	for _, row := range rowsBefore {
		if row.StepName == types.StepTest {
			if err := d.FailStep(row.ID, "recorded test failure", 5); err != nil {
				t.Fatal(err)
			}
		}
	}
	rowsBefore, err = d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := json.Marshal(rowsBefore)
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(d, p, nil)
	gateRefsBefore := gitOutput(t, p.RepoDir(rmt.ID), "for-each-ref", "--format=%(refname) %(objectname)")

	result, err := mgr.HandleRepublishPublication(t.Context(), ipc.RepublishPublicationParams{RepoID: rmt.ID, RunID: run.ID, HeadSHA: head})
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
	// Decode the generated public attestation contract, not implementation
	// source: every recorded status must survive, with no invented rows.
	_, payload, found := strings.Cut(string(body), "<!-- no-mistakes-pipeline-attestation:v1 ")
	if !found {
		t.Fatalf("published body has no attestation:\n%s", body)
	}
	payload, _, found = strings.Cut(payload, " -->")
	if !found {
		t.Fatal("published attestation is unterminated")
	}
	var attestation struct {
		HeadSHA string `json:"head_sha"`
		Steps   []struct {
			Step   types.StepName   `json:"step"`
			Status types.StepStatus `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
		t.Fatalf("decode published attestation: %v", err)
	}
	if attestation.HeadSHA != head || len(attestation.Steps) != len(rowsBefore) {
		t.Fatalf("published attestation changed head or step inventory: %+v", attestation)
	}
	for i, row := range rowsBefore {
		if got := attestation.Steps[i]; got.Step != row.StepName || got.Status != row.Status {
			t.Fatalf("published step %d = %+v, want %s:%s", i, got, row.StepName, row.Status)
		}
	}
	rowsAfter, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(rowsAfter)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("continuation changed recorded step rows:\nbefore: %s\nafter: %s", beforeJSON, afterJSON)
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
	if _, err := os.Stat(filepath.Join(p.WorktreeDir(rmt.ID, run.ID), "republish")); !os.IsNotExist(err) {
		t.Fatalf("republish worktree survived: %v", err)
	}
	// R3: the operation-private refs are gone and the gate's refs (including
	// its origin/ tracking refs) are byte-identical to before the continuation.
	for _, ref := range republishPrivateRefs(run.ID) {
		if out := strings.TrimSpace(gitAnyOutput(t, p.RepoDir(rmt.ID), "for-each-ref", "--format=%(refname)", ref)); out != "" {
			t.Fatalf("private ref %s survived: %s", ref, out)
		}
	}
	if gateRefsAfter := gitOutput(t, p.RepoDir(rmt.ID), "for-each-ref", "--format=%(refname) %(objectname)"); gateRefsAfter != gateRefsBefore {
		t.Fatalf("gate refs moved during continuation:\nbefore:\n%s\nafter:\n%s", gateRefsBefore, gateRefsAfter)
	}

	for _, tc := range []struct {
		name   string
		branch string
		want   string
	}{
		{"expired-auth", "unauthenticated", "authentication expired"},
		{"base-branch", "main", "PR step was skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_CLI_MODE", "ci-gh")
			t.Setenv("FAKE_CLI_AUTH_ERR", "authentication expired")
			// R1 verifies the live source branch before the PR step, so each
			// case needs its own reachable remote carrying that branch.
			rrepo, rhead := republishReachableFixture(t, p, d, "republish-skip-"+tc.branch, tc.branch)
			run := terminalRunWithFailedPR(t, d, rrepo.ID, tc.branch, rhead)
			result, err := mgr.HandleRepublishPublication(t.Context(), ipc.RepublishPublicationParams{RepoID: rrepo.ID, RunID: run.ID, HeadSHA: rhead})
			if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
				t.Fatalf("result = %+v, error = %v; want refusal containing %q", result, err, tc.want)
			}
			stored, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.PRURL != nil || stored.Status != types.RunFailed {
				t.Fatalf("skipped publication changed run: %+v", stored)
			}
			if _, err := os.Stat(filepath.Join(p.WorktreeDir(rrepo.ID, run.ID), "republish")); !os.IsNotExist(err) {
				t.Fatalf("skipped publication worktree survived: %v", err)
			}
		})
	}
}

func TestRepublishPublication_WaitsForTerminalCleanup(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, head := setupTestGitRepo(t, p, d, "republish-cleanup")
	run := terminalRunWithFailedPR(t, d, repo.ID, "feature", head)
	mgr := NewRunManager(d, p, nil)
	done := make(chan struct{})
	mgr.dones[run.ID] = done
	defer close(done)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := mgr.HandleRepublishPublication(ctx, ipc.RepublishPublicationParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: head})
	if result != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("result = %+v, error = %v; want deadline waiting for cleanup", result, err)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, run.ID)); !os.IsNotExist(err) {
		t.Fatalf("continuation created worktree before cleanup completed: %v", err)
	}
}
