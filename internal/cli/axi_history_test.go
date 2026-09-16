package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiPreserveHistory_RejectsUnsupportedDaemonWithoutFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{}) // models the old RPC surface
	ordinaryLookup := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		ordinaryLookup = true
		return nil, nil
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runAxiPreserveHistory(cmd, false, nil, "preserve history", "main", fx.head, "", "", time.Minute)
	if err == nil || !strings.Contains(out.String(), "start_history_run") || !strings.Contains(out.String(), "no unconstrained fallback") {
		t.Fatalf("unsupported daemon did not fail closed: %v\n%s", err, &out)
	}
	if ordinaryLookup {
		t.Fatal("unsupported constrained launch fell back to ordinary launch")
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("unsupported launch changed the caller head")
	}
}

func TestAxiPreserveHistory_RejectsInvalidControlsBeforeOpeningEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, base, sha, nonce, generation, want string
		skip                                     []types.StepName
	}{
		{name: "empty", base: "main", want: "full nonzero"},
		{name: "moving-ref", base: "main", sha: "origin/main", want: "full nonzero"},
		{name: "missing-branch", sha: strings.Repeat("a", 40), want: "--base-branch"},
		{name: "skip", base: "main", sha: strings.Repeat("a", 40), skip: []types.StepName{types.StepRebase}, want: "--skip"},
		{name: "incomplete-proof", base: "main", sha: strings.Repeat("a", 40), nonce: "n", want: "supplied together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			err := runAxiPreserveHistory(cmd, false, tc.skip, "intent", tc.base, tc.sha, tc.nonce, tc.generation, time.Minute)
			if err == nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("want %q: %v\n%s", tc.want, err, &out)
			}
		})
	}
}

// An active run whose head has not moved may be superseded by a fresh push -
// unless it pins a preserve-history integration. The agent-side sequence is an
// amend (or rebase) of the local commit followed by `axi run`: the guard must
// refuse before the gate push, because the daemon can only refuse after the
// post-receive hook has already moved the gate branch.
func TestAxiFreshRunOwnershipGuardBlocksSupersedingAProtectedRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protected bool
		published bool
		change    string
		wantBlock bool
	}{
		{name: "ordinary_unmoved_run_stays_supersedable", change: "amend"},
		{name: "protected_unmoved_run_blocks_an_amend", protected: true, change: "amend", wantBlock: true},
		{name: "protected_published_run_blocks_a_follow_up_commit", protected: true, published: true, change: "follow-up", wantBlock: true},
		{name: "protected_published_run_blocks_an_amend", protected: true, published: true, change: "amend", wantBlock: true},
		{name: "caller_at_the_protected_head_may_reattach", protected: true, published: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NM_HOME", filepath.Join(t.TempDir(), "nm-home"))
			root := t.TempDir()
			local := filepath.Join(root, "operator")
			cliGit(t, root, "init", "-b", "main", local)
			cliGit(t, local, "config", "user.name", "Test")
			cliGit(t, local, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cliGit(t, local, "add", "file.txt")
			cliGit(t, local, "commit", "-m", "base")
			base := cliGit(t, local, "rev-parse", "HEAD")
			cliGit(t, local, "checkout", "-b", "feature/pinned")
			if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("integration\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cliGit(t, local, "commit", "-am", "integration")
			submitted := cliGit(t, local, "rev-parse", "HEAD")

			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			registeredRoot, err := git.FindGitRoot(local)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.InsertRepo(registeredRoot, filepath.Join(root, "remote.git"), "main")
			if err != nil {
				t.Fatal(err)
			}
			gate := p.RepoDir(repo.ID)
			cliGit(t, filepath.Dir(gate), "init", "--bare", gate)
			cliGit(t, local, "push", gate, "refs/heads/feature/pinned:refs/heads/feature/pinned")
			pin := ""
			if tc.protected {
				pin = base
			}
			run, err := database.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature/pinned", submitted, base, nil, "", "", "", "main", pin)
			if err != nil {
				t.Fatal(err)
			}
			if tc.published {
				if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
					HeadSHA: submitted, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature/pinned",
				}); err != nil {
					t.Fatal(err)
				}
			}

			switch tc.change {
			case "amend":
				cliGit(t, local, "commit", "--amend", "-m", "rewritten integration")
			case "follow-up":
				cliGit(t, local, "commit", "--allow-empty", "-m", "follow-up during CI repair")
			}
			chdir(t, local)
			env := &axiEnv{p: p, d: database, repo: repo, cfg: config.DefaultGlobalConfig()}
			state := freshRunBranchOwnershipState(context.Background(), env)
			if blocked := state != nil; blocked != tc.wantBlock {
				t.Fatalf("blocked = %v, want %v (state %+v)", blocked, tc.wantBlock, state)
			}
			if got := cliGit(t, gate, "rev-parse", "refs/heads/feature/pinned"); got != submitted {
				t.Fatalf("gate branch = %s, want the pinned integration %s", got, submitted)
			}
		})
	}
}
