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

func TestExplicitPRIntegrationFetchDoesNotRedirectPush(t *testing.T) {
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
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", "https://github.com/upstream/widgets.git")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	if err := fetchRunUpstreamBranch(context.Background(), sctx, "main"); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "origin/main"); got != parentHead {
		t.Fatalf("integration fetch read fork: %s want %s", got, parentHead)
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
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream-only"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "upstream advancement")
	parentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", parent, "main")
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
	if got := gitCmd(t, dir, "rev-parse", "refs/remotes/origin/main"); got != parentHead {
		t.Fatalf("integration ref = %s want %s", got, parentHead)
	}
	if _, err := gitRun(dir, "merge-base", "--is-ancestor", parentHead, "HEAD"); err != nil {
		t.Fatalf("branch was not rebased onto the integration branch: %v", err)
	}
}

func gitRun(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}
