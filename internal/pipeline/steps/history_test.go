package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The submitted commit has current main FIRST and the preserved feature SECOND.
// A tree-equivalent rebase is not a substitute for either exact parent.
func newHistoryFixture(t *testing.T, revalidate bool) (*ciRepairFixture, string, []string) {
	t.Helper()
	f := newCIRepairFixture(t, revalidate, nil)
	preserved := f.headSHA
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "main.txt", "new main\n")
	gitCmd(t, f.dir, "add", "main.txt")
	gitCmd(t, f.dir, "commit", "-m", "main advance")
	base := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "merge", "--no-ff", "feature", "-m", "explicit integration")
	gitCmd(t, f.dir, "checkout", "-B", "feature")
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	run, err := f.sctx.DB.InsertRunWithIntentAndLaunchNonce(f.sctx.Repo.ID, "feature", head, base, nil, "history-nonce", "generation", "digest", "main", base)
	if err != nil {
		t.Fatal(err)
	}
	run.PRURL = f.sctx.Run.PRURL
	f.sctx.Run = run
	recordReviewApproval(t, f.sctx, head)
	for i, env := range f.sctx.Env {
		f.sctx.Env[i] = strings.ReplaceAll(env, preserved, head)
	}
	f.headSHA = head
	return f, base, []string{base, preserved}
}

func spyHistoryGit(t *testing.T, f *ciRepairFixture) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "git")
	linkTestBinary(t, bin, "gh")
	log := filepath.Join(t.TempDir(), "commands.log")
	var keep []string
	for _, entry := range f.sctx.Env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "PATH" && key != "FAKE_CLI_MODE" && key != "FAKE_CLI_REAL_GIT" && key != "FAKE_CLI_LOG" {
			keep = append(keep, entry)
		}
	}
	f.sctx.Env = append(keep, fakeCLIEnv(bin, map[string]string{
		// With no interloper configured, this is just a real-Git recorder.
		"FAKE_CLI_MODE": "ci-gh-with-intervening-push", "FAKE_CLI_REAL_GIT": realGit, "FAKE_CLI_LOG": log,
	})...)
	return log
}

func assertHistoryUntouched(t *testing.T, f *ciRepairFixture, expectedParents []string) {
	t.Helper()
	if got := parents(t, f.dir, f.headSHA); !reflect.DeepEqual(got, expectedParents) {
		t.Fatalf("source parents = %v, want %v", got, expectedParents)
	}
	gitCmd(t, f.dir, "merge-base", "--is-ancestor", f.headSHA, "HEAD")
	for _, parent := range expectedParents {
		gitCmd(t, f.dir, "merge-base", "--is-ancestor", parent, "HEAD")
	}
}

func TestPreserveHistory_InitialPublicationAndAdditiveCIRepair(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "revalidate"}[revalidate], func(t *testing.T) {
			f, _, originalParents := newHistoryFixture(t, revalidate)
			log := spyHistoryGit(t, f)
			// Exercise a new branch first, and an existing branch during repair.
			gitCmd(t, f.upstream, "update-ref", "-d", "refs/heads/feature")
			if _, err := (&RebaseStep{}).Execute(f.sctx); err != nil {
				t.Fatal(err)
			}
			if f.localHead(t) != f.headSHA {
				t.Fatal("initial validation changed the submitted integration")
			}
			if err := publishRunHead(f.sctx, f.headSHA, "", nil); err != nil {
				t.Fatal(err)
			}
			if f.remoteHead(t) != f.headSHA {
				t.Fatal("initial publication did not publish the exact integration")
			}
			calls := 0
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				writeCIFix(opts.CWD)
				return &agent.Result{Output: []byte(`{"summary":"additive CI correction"}`)}, nil
			}}
			repair, err := (&CIStep{}).autoFixCI(f.sctx, &forgejoLogTestHost{}, &scm.PR{Number: "42", BaseBranch: "main"}, ciTargetsFor([]string{"test"}, false))
			if err != nil || calls != 1 || !repair.HeadAdvanced || repair.Revalidate != revalidate {
				t.Fatalf("repair=%+v calls=%d err=%v", repair, calls, err)
			}
			assertHistoryUntouched(t, f, originalParents)
			if revalidate {
				if f.remoteHead(t) != f.headSHA {
					t.Fatal("held repair published before revalidation")
				}
				// Stand in for completed revalidation, then exercise its real
				// publication path with the same durable integration pins.
				recordReviewApproval(t, f.sctx, f.localHead(t))
				if err := publishRunHead(f.sctx, f.localHead(t), "", nil); err != nil {
					t.Fatal(err)
				}
			}
			if f.remoteHead(t) != f.localHead(t) || f.remoteHead(t) == f.headSHA {
				t.Fatal("additive correction was not published")
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != f.headSHA || run.PreserveHistoryBaseSHA == nil {
				t.Fatalf("repair lost immutable pins: %+v, %v", run, err)
			}
			commands, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			pushes := 0
			for _, line := range strings.Split(string(commands), "\n") {
				if strings.HasPrefix(line, "push ") {
					pushes++
					if strings.Contains(line, "--force") || strings.Contains(line, "+refs") {
						t.Fatalf("forced publication: %s", line)
					}
				}
				if strings.HasPrefix(line, "rebase ") || strings.HasPrefix(line, "merge ") {
					t.Fatalf("automatic integration: %s", line)
				}
			}
			if pushes != 2 {
				t.Fatalf("recorded %d publications, want 2:\n%s", pushes, commands)
			}
		})
	}
}

func TestPreserveHistory_MovedBaseRefusesBeforeIntegrationOrFixer(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			f, base, originalParents := newHistoryFixture(t, false)
			f.sctx.Config.Rebase.Strategy = strategy
			log := spyHistoryGit(t, f)
			// A commit created without checking out or changing the submitted head.
			tree := gitCmd(t, f.dir, "rev-parse", base+"^{tree}")
			moved := gitCmd(t, f.dir, "commit-tree", tree, "-p", base, "-m", "later main")
			gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/main")
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("agent launched despite a moved pinned base")
				return nil, nil
			}}
			for _, operation := range []func() error{
				func() error { _, err := (&RebaseStep{}).Execute(f.sctx); return err },
				func() error {
					_, err := (&CIStep{}).autoFixCI(f.sctx, nil, nil, ciTargetsFor([]string{"test"}, false))
					return err
				},
				func() error { return publishRunHead(f.sctx, f.headSHA, "", nil) },
			} {
				if err := operation(); !errors.Is(err, ErrHistoryConstraint) || !strings.Contains(err.Error(), "moved") {
					t.Fatalf("expected explicit pre-mutation refusal, got %v", err)
				}
			}
			assertHistoryUntouched(t, f, originalParents)
			assertRestoredToReviewedHead(t, f.dir, f.headSHA)
			commands, _ := os.ReadFile(log)
			for _, command := range []string{"\nrebase ", "\nmerge ", "\npush ", "\nreset "} {
				if strings.Contains("\n"+string(commands), command) {
					t.Fatalf("prohibited operation occurred:\n%s", commands)
				}
			}
		})
	}
}

func TestPreserveHistory_CIMergeConflictNeverLaunchesAResolver(t *testing.T) {
	f, _, originalParents := newHistoryFixture(t, false)
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("merge conflict resolver launched")
		return nil, nil
	}}
	head := f.remoteHead(t)
	_, err := (&CIStep{}).autoFixCI(f.sctx, nil, nil, ciTargetsFor(nil, true))
	if !errors.Is(err, ErrHistoryConstraint) || !strings.Contains(err.Error(), "merge-conflict") {
		t.Fatalf("expected no-rebase refusal, got %v", err)
	}
	assertHistoryUntouched(t, f, originalParents)
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
	if f.remoteHead(t) != head {
		t.Fatal("merge-conflict refusal changed the remote")
	}
}

func TestPreserveHistory_RefusesLeaseAuthorizedRewrite(t *testing.T) {
	f, _, originalParents := newHistoryFixture(t, false)
	// Model a known prior published sibling. Legacy lease logic permits
	// replacing it; the new policy must refuse even with exact lease evidence.
	tree := gitCmd(t, f.dir, "rev-parse", f.headSHA+"^{tree}")
	sibling := gitCmd(t, f.dir, "commit-tree", tree, "-p", originalParents[1], "-m", "prior published correction")
	gitCmd(t, f.dir, "push", "origin", sibling+":refs/heads/feature")
	if err := f.sctx.DB.UpdateRunPushBinding(f.sctx.Run.ID, db.PushBinding{HeadSHA: sibling, Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	log := spyHistoryGit(t, f)
	if err := publishRunHead(f.sctx, f.headSHA, "", nil); !errors.Is(err, ErrHistoryConstraint) || !strings.Contains(err.Error(), "force-with-lease") {
		t.Fatalf("expected force refusal, got %v", err)
	}
	if f.remoteHead(t) != sibling {
		t.Fatal("refused publication overwrote the remote")
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
	commands, _ := os.ReadFile(log)
	if strings.Contains("\n"+string(commands), "\npush ") {
		t.Fatalf("refusal reached push:\n%s", commands)
	}
}

func TestPreserveHistory_InvalidPinsFailClosed(t *testing.T) {
	f, _, _ := newHistoryFixture(t, false)
	for _, bad := range []string{"", "main", "1234567", strings.Repeat("0", 40)} {
		f.sctx.Run.PreserveHistoryBaseSHA = &bad
		if err := AssertHistoryPolicy(f.sctx); !errors.Is(err, ErrHistoryConstraint) {
			t.Fatalf("invalid pin %q silently opted out: %v", bad, err)
		}
	}
}

// Every later step scopes its diff through origin/<base>, so the preserve-history
// path must still refresh that remote-tracking ref even though it rebases nothing.
func TestPreserveHistory_InitialValidationRefreshesThePinnedBaseRef(t *testing.T) {
	f, base, originalParents := newHistoryFixture(t, false)
	stale := gitCmd(t, f.dir, "rev-parse", base+"^")
	gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/main", stale)
	if _, err := (&RebaseStep{}).Execute(f.sctx); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "refs/remotes/origin/main"); got != base {
		t.Fatalf("origin/main = %s, want the pinned base %s; later steps would review the base branch's own commits", got, base)
	}
	assertHistoryUntouched(t, f, originalParents)
}
