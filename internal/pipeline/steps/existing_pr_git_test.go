package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// refs/remotes/origin/ lives in the gate repository every worktree and run of
// the registered repository shares, so an explicit target's foreign integration
// branch must land elsewhere: an ordinary run that reads origin/<default> must
// still see its own repository, including after an explicit run fetched.
func TestExplicitPRIntegrationFetchLeavesSharedOriginRefsAlone(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	parent := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	fork := t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	forkMain := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	parentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "main")
	gitCmd(t, dir, "reset", "--hard", forkMain)
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	if err := FetchRunUpstreamBranch(context.Background(), sctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", runIntegrationRef(sctx, "main")); got != parentHead {
		t.Fatalf("integration fetch read fork: %s want %s", got, parentHead)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/remotes/origin/main"); got != forkMain {
		t.Fatalf("explicit integration fetch moved the shared origin ref to %s, want %s", got, forkMain)
	}
	ordinary := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	if err := FetchRunUpstreamBranch(context.Background(), ordinary, "main"); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "origin/main"); got != forkMain {
		t.Fatalf("ordinary run integrated with the explicit target's repository: %s want %s", got, forkMain)
	}
	if got := resolvePushURL(sctx); got != fixtureSourceURL {
		t.Fatalf("push target changed: %s", got)
	}
	if got := gitCmd(t, fork, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("fetch mutated source: %s", got)
	}
}

// The source branch of an explicit PR lives in the push repository while the
// integration refs come from the PR's repository, so its remote-tracking ref
// must be fully qualified and kept out of origin/<branch>. An unqualified
// destination writes refs/heads/origin/<branch> into the shared gate instead,
// which then shadows the real tracking ref for every later run on the branch.
func TestExplicitPRRebaseTracksSourceBranchOutsideOrigin(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	parent := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	fork := t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	forkMain := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	parentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "main")
	gitCmd(t, dir, "reset", "--hard", forkMain)
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if out, err := gitRun(dir, "rev-parse", "--verify", "--quiet", "refs/heads/origin/feature"); err == nil {
		t.Fatalf("fetch created a stray local branch refs/heads/origin/feature at %s", strings.TrimSpace(out))
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/remotes/no-mistakes-push/feature"); got != head {
		t.Fatalf("source branch tracking ref = %s want %s", got, head)
	}
	if got := gitCmd(t, dir, "rev-parse", runIntegrationRef(sctx, "main")); got != parentHead {
		t.Fatalf("integration ref = %s want %s", got, parentHead)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/remotes/origin/main"); got != forkMain {
		t.Fatalf("rebase moved the shared origin ref to %s, want %s", got, forkMain)
	}
	if _, err := gitRun(dir, "merge-base", "--is-ancestor", parentHead, "HEAD"); err != nil {
		t.Fatalf("branch was not rebased onto the integration branch: %v", err)
	}
}

// Every gate measures its diff from the branch the run integrates with. For an
// explicit target that is the pull request's base in the PR's repository: the
// registered origin/<default> is the contributor's fork, and measuring from it
// hands the review and the fix agent upstream commits the contributor never
// wrote - which the pipeline then force-pushes back onto the PR's source branch.
func TestExplicitPRDiffBaseIsTheUpstreamBaseNotTheForkDefault(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	parent := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	fork := t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	forkMain := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	parentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "main")
	gitCmd(t, dir, "reset", "--hard", forkMain)
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	ctx := context.Background()
	// The daemon establishes the integration ref at launch, before any step.
	if err := FetchRunUpstreamBranch(ctx, sctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA, err := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if baseSHA != parentHead {
		t.Fatalf("diff base = %s, want the pull request's base %s (fork default is %s)", baseSHA, parentHead, forkMain)
	}
	changed := gitCmd(t, dir, "diff", "--name-only", baseSHA, "HEAD")
	if strings.Contains(changed, "upstream.txt") {
		t.Fatalf("gates would review and edit upstream commits:\n%s", changed)
	}
}

// CI re-reads the pull request's live base and hands it to the repair helpers,
// so a maintainer retargeting the PR mid-run must move the diff base with the
// rebase target. A merge base measured from the branch stored at launch would
// hand the fix agent a diff from the old base while rebasing onto the new one.
func TestExplicitPRDiffBaseFollowsTheCallerSelectedUpstreamBranch(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	parent := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	fork := t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	gitCmd(t, dir, "push", parent, "main")
	if err := os.WriteFile(filepath.Join(dir, "release.txt"), []byte("release-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "release advancement")
	releaseHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "HEAD:refs/heads/release/2.0")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", releaseHead)
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	// The run was launched against main; the maintainer has since retargeted.
	launchBase := "main"
	if err := sctx.DB.SetRunPRBaseBranch(sctx.Run.ID, launchBase); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRBaseBranch = &launchBase
	ctx := context.Background()
	if err := FetchRunUpstreamBranch(ctx, sctx, launchBase); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	// CI repair is the one caller that re-reads the live base, and it refreshes
	// that branch before measuring against it.
	baseSHA, rebaseSHA, err := resolveCIRepairBases(ctx, sctx, "release/2.0")
	if err != nil {
		t.Fatal(err)
	}
	if rebaseSHA != releaseHead {
		t.Fatalf("CI repair would rebase onto %s, want the live base %s", rebaseSHA, releaseHead)
	}
	if baseSHA != releaseHead {
		t.Fatalf("diff base = %s, want the live base %s", baseSHA, releaseHead)
	}
	changed := gitCmd(t, dir, "diff", "--name-only", baseSHA, "HEAD")
	if strings.Contains(changed, "release.txt") {
		t.Fatalf("diff measured from the launch base still carries the live base's commits:\n%s", changed)
	}
}

// A retargeted pull request whose new base cannot be read - the fetch failed,
// or the branch is gone - stops the run. Measuring from the branch the run was
// launched against would hand the fix agent every commit the new base has
// accumulated since, and measuring from the run's own recorded base (its
// submitted head) would diff HEAD against itself: an empty diff no step
// reports as a failure. Neither substitution is acceptable.
func TestExplicitPRUnreadableIntegrationBaseStopsInsteadOfSubstituting(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	parent := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	fork := t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	parentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "main")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", parentHead)
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	launchBase := "main"
	if err := sctx.DB.SetRunPRBaseBranch(sctx.Run.ID, launchBase); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRBaseBranch = &launchBase
	ctx := context.Background()
	if err := FetchRunUpstreamBranch(ctx, sctx, launchBase); err != nil {
		t.Fatal(err)
	}
	// An explicit launch records the submitted head as the run's base.
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	sctx.Run.BaseSHA = sctx.Run.HeadSHA
	// CI repair asks for both commits at once: neither may be answered with the
	// run's own head, which for an associated run is its recorded base.
	diffBase, rebaseSHA, err := resolveCIRepairBases(ctx, sctx, "release/never-published")
	if err == nil {
		t.Fatalf("CI repair proceeded from a substituted base %s and would rebase onto %s", diffBase, rebaseSHA)
	}
	if rebaseSHA == sctx.Run.HeadSHA || diffBase == sctx.Run.HeadSHA {
		t.Fatalf("CI repair answered with the branch's own head: base=%s rebase=%s", diffBase, rebaseSHA)
	}
	if baseSHA, baseErr := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, "release/never-published"); baseErr == nil {
		t.Fatalf("measured the change from a substituted base %s", baseSHA)
	}
	if launched, launchedErr := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, launchBase); launchedErr != nil || launched != parentHead {
		t.Fatalf("the branch the run was launched against stopped resolving: %s, %v", launched, launchedErr)
	}
}

func gitRun(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}
