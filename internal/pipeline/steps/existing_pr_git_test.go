package steps

import (
	"context"
	"os"
	"path/filepath"
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
