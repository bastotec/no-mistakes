package branchsync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// supersededFixture builds two runs' heads around one bare gate: an older
// terminal unpublished head, and a newer run's pushed head sitting at the
// gate branch. The shape is the divergent one - a rebase rewrote the older
// head's SHAs while replaying its patches - so no ancestor relation exists
// between them even though nothing was lost.
type supersededFixture struct {
	t        *testing.T
	ctx      context.Context
	work     string
	gate     string
	older    *db.Run
	newer    *db.Run
	branch   string
	pushed   string
	base     string
	olderSHA string
}

func newSupersededFixture(t *testing.T) *supersededFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	gate := filepath.Join(root, "gate.git")
	mustRun(t, "", "init", work)
	mustRun(t, work, "config", "user.email", "test@test.com")
	mustRun(t, work, "config", "user.name", "Test")
	mustWrite(t, filepath.Join(work, "feature.txt"), "one\ntwo\nthree\nfour\nfive\nsix\n")
	mustRun(t, work, "add", "feature.txt")
	mustRun(t, work, "commit", "-m", "base")
	base := mustRun(t, work, "rev-parse", "HEAD")

	const branch = "feature/sync"
	// The older run's unpublished head: the operator's own change.
	mustRun(t, work, "checkout", "-b", "older", base)
	mustWrite(t, filepath.Join(work, "feature.txt"), "one\ntwo\nthree\nfour-priv\nfive\nsix\n")
	mustRun(t, work, "add", "feature.txt")
	mustRun(t, work, "commit", "-m", "operator work")
	olderSHA := mustRun(t, work, "rev-parse", "HEAD")

	// The newer run's pushed head: the same change replayed under new SHAs
	// and then superseded in place by the pipeline's own fix.
	mustRun(t, work, "checkout", "-b", "pushed", base)
	mustWrite(t, filepath.Join(work, "upstream.txt"), "upstream advance\n")
	mustRun(t, work, "add", "upstream.txt")
	mustRun(t, work, "commit", "-m", "upstream advance")
	mustWrite(t, filepath.Join(work, "feature.txt"), "one\ntwo\nthree\nfour-priv\nfive\nsix\n")
	mustRun(t, work, "add", "feature.txt")
	mustRun(t, work, "commit", "-m", "operator work replayed")
	mustWrite(t, filepath.Join(work, "feature.txt"), "one\ntwo\nthree\nfour-fixed\nfive\nsix\n")
	mustRun(t, work, "commit", "-am", "pipeline fix supersedes the operator line")
	pushed := mustRun(t, work, "rev-parse", "HEAD")

	mustRun(t, "", "init", "--bare", gate)
	// The older run's preserved head is anchored in the gate exactly as
	// custody recovery leaves it, and the newer push moved the branch.
	mustRun(t, work, "push", gate, olderSHA+":"+custody.RecoveryRef("older-run"))
	mustRun(t, work, "push", gate, pushed+":refs/heads/"+branch)

	kind := "upstream"
	fingerprint := TargetFingerprint("ssh://example.invalid/owner/repo.git")
	ref := "refs/heads/" + branch
	submitted := base
	older := &db.Run{
		ID: "older-run", RepoID: "repo", Branch: branch, Status: types.RunFailed,
		HeadSHA: olderSHA, SubmittedHeadSHA: &submitted,
		PushTargetKind: &kind, PushTargetFingerprint: &fingerprint, PushRef: &ref,
	}
	newer := &db.Run{
		ID: "newer-run", RepoID: "repo", Branch: branch, Status: types.RunCompleted,
		HeadSHA: pushed, LastPushedSHA: &pushed,
		PushTargetKind: &kind, PushTargetFingerprint: &fingerprint, PushRef: &ref,
	}
	return &supersededFixture{
		t: t, ctx: ctx, work: work, gate: gate,
		older: older, newer: newer, branch: branch,
		pushed: pushed, base: base, olderSHA: olderSHA,
	}
}

// TestSupersededUnpublishedRunAcceptsPatchEquivalentRebasedPush pins the
// content-identity half of the supersession proof: a newer push that rebased
// the older preserved head's changes under new SHAs (and then superseded them
// with its own fix) retains that head's work even though it contains none of
// its commit objects, so the older run stops being authoritative.
func TestSupersededUnpublishedRunAcceptsPatchEquivalentRebasedPush(t *testing.T) {
	f := newSupersededFixture(t)
	if _, ancErr := gitpkg.Run(f.ctx, f.gate, "merge-base", "--is-ancestor", f.olderSHA, f.pushed); ancErr == nil {
		t.Fatal("fixture: the older head must not be an ancestor of the pushed head")
	}
	svc := &Service{GateDir: f.gate}
	if !svc.supersededUnpublishedRun(f.ctx, f.older, f.newer, f.branch) {
		t.Fatal("patch-equivalent rebased push did not supersede the older unpublished head")
	}
}

// TestSupersededUnpublishedRunKeepsHeadWithUnretainedWork: the newer push
// dropped the older head's change entirely, so nothing proves retention and
// the older run must stay authoritative.
func TestSupersededUnpublishedRunKeepsHeadWithUnretainedWork(t *testing.T) {
	f := newSupersededFixture(t)
	// Rebuild the pushed head without the operator's change: only the base
	// advance and the pipeline fix land, and the fix no longer replays it.
	mustRun(t, f.work, "checkout", "-b", "dropped", f.base)
	mustWrite(t, filepath.Join(f.work, "upstream.txt"), "upstream advance\n")
	mustRun(t, f.work, "add", "upstream.txt")
	mustRun(t, f.work, "commit", "-m", "upstream advance")
	mustWrite(t, filepath.Join(f.work, "feature.txt"), "one\ntwo\nthree\nfour-other\nfive\nsix\n")
	mustRun(t, f.work, "commit", "-am", "pipeline rewrites the line without the operator change")
	dropped := mustRun(t, f.work, "rev-parse", "HEAD")
	mustRun(t, f.work, "push", f.gate, "+"+dropped+":refs/heads/"+f.branch)
	head := dropped
	f.newer.HeadSHA = head
	f.newer.LastPushedSHA = &head

	svc := &Service{GateDir: f.gate}
	if svc.supersededUnpublishedRun(f.ctx, f.older, f.newer, f.branch) {
		t.Fatal("push that dropped the older head's work superseded it anyway")
	}
}

// TestSupersededUnpublishedRunFailsClosedOnStaleGateHead: the gate branch no
// longer equals the newer push binding, so the read-only evidence is stale and
// the older run stays authoritative regardless of content.
func TestSupersededUnpublishedRunFailsClosedOnStaleGateHead(t *testing.T) {
	f := newSupersededFixture(t)
	mustRun(t, f.work, "checkout", "-b", "elsewhere", f.base)
	mustWrite(t, filepath.Join(f.work, "elsewhere.txt"), "elsewhere\n")
	mustRun(t, f.work, "add", "elsewhere.txt")
	mustRun(t, f.work, "commit", "-m", "elsewhere work")
	elsewhere := mustRun(t, f.work, "rev-parse", "HEAD")
	mustRun(t, f.work, "push", f.gate, "+"+elsewhere+":refs/heads/"+f.branch)

	svc := &Service{GateDir: f.gate}
	if svc.supersededUnpublishedRun(f.ctx, f.older, f.newer, f.branch) {
		t.Fatal("stale gate head still read as superseding evidence")
	}
}
