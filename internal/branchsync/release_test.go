package branchsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewriteRemoteForRelease simulates the stranded shape from defect 1
// (2026-09-17): the remote branch was rewritten (or deleted) by someone else
// after this terminal run pushed, so the persisted push binding can never be
// satisfied by an ordinary push again.
func rewriteRemoteForRelease(t *testing.T, f *recoverFixture) {
	t.Helper()
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, f.t.TempDir(), "-c", "core.autocrlf=false", "clone", "-b", "feature/recover", f.remote, writer)
	configureIdentity(t, writer)
	// Rewrite history instead of appending: the new remote head does not
	// descend from the pushed binding, which is what strands it for good.
	mustRun(t, writer, "reset", "--hard", "HEAD~1")
	mustWrite(t, filepath.Join(writer, "file.txt"), "rewritten by someone else\n")
	mustRun(t, writer, "commit", "-am", "external rewrite")
	mustRun(t, writer, "push", "--force", f.remote, "HEAD:refs/heads/feature/recover")
}

// bindTerminalRun records the fixture's terminal run as the publisher of the
// submitted head to the fixture's remote, with the run's final head exactly
// the pushed head - the stranded shape of defect 1. The fixture's later
// pipeline commits stay in the gate as an independently moved gate head that
// release must anchor rather than lose.
func bindTerminalRun(t *testing.T, f *recoverFixture) {
	t.Helper()
	mustRun(t, f.local, "push", f.remote, "refs/heads/feature/recover:refs/heads/feature/recover")
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
}

// TestReleaseStrandedTerminalBindingAfterRemoteRewrite is the defect-1 happy
// path: every blocked state now carries a next_action, and the sanctioned
// release for a terminal run's stale push binding anchors the pipeline heads,
// re-points the gate branch to the operator's head, stamps custody, and never
// touches the rewritten remote.
func TestReleaseStrandedTerminalBindingAfterRemoteRewrite(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)
	rewriteRemoteForRelease(t, f)

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("stranded classification = %#v", state)
	}
	// Defect 1: the blocked state must offer an exit, and for a terminal
	// run's stale binding that exit is the release.
	if state.NextAction == nil || state.NextAction.Code != "release_binding" || state.NextAction.Command != "no-mistakes axi sync --release" {
		t.Fatalf("stranded next action = %#v", state.NextAction)
	}

	released := f.service.Release(f.ctx)
	if !released.Released || !released.Changed {
		t.Fatalf("release = %#v", released)
	}
	if released.NextAction == nil || released.NextAction.Code != "run_pipeline" {
		t.Fatalf("released next action = %#v", released.NextAction)
	}
	// The gate branch now tracks the operator's head, so an ordinary push
	// works again without ever force-moving the rewritten remote.
	localHead := mustRun(t, f.local, "rev-parse", "refs/heads/feature/recover")
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != localHead {
		t.Fatalf("gate branch after release = %s, want the local head %s", got, localHead)
	}
	// The independently moved gate head stays anchored and recoverable.
	if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryGateRef(f.run.ID)+"^{commit}"); got != f.preserved {
		t.Fatalf("gate recovery anchor after release = %s, want the preserved head %s", got, f.preserved)
	}
	if !f.custodyReturned() {
		t.Fatal("release did not stamp custody")
	}
	// The rewritten remote is deliberately left alone.
	remoteHead := mustRun(t, f.local, "rev-parse", f.remote+"/refs/heads/feature/recover")
	if remoteHead == f.submitted {
		t.Fatal("fixture remote was not rewritten; test proves nothing")
	}

	// Idempotent: releasing an already-released binding is a no-op.
	again := f.service.Release(f.ctx)
	if !again.Recovered || again.Changed || again.Released {
		t.Fatalf("second release = %#v, want an idempotent no-op", again)
	}
}

// TestReleaseRefusesWhileRunIsActive pins the precondition: an active run
// still owns the binding, so release points back at driving that run.
func TestReleaseRefusesWhileRunIsActive(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunRunning)
	bindTerminalRun(t, f)
	rewriteRemoteForRelease(t, f)

	state := f.service.Release(f.ctx)
	if state.Released || state.Changed {
		t.Fatalf("active run release = %#v, want a refusal", state)
	}
	if state.Safety != "blocked_release_run_active" {
		t.Fatalf("active refusal safety = %s", state.Safety)
	}
	if state.NextAction == nil || state.NextAction.Code != "continue_active_run" {
		t.Fatalf("active refusal next action = %#v", state.NextAction)
	}
	if f.custodyReturned() {
		t.Fatal("refused release stamped custody")
	}
}

// TestReleaseNotApplicableWhenRemoteMatchesBinding pins the other
// precondition: when the live remote still equals the binding, ordinary
// synchronization applies and release would discard a valid binding.
func TestReleaseNotApplicableWhenRemoteMatchesBinding(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)

	state := f.service.Release(f.ctx)
	if state.Released || state.Changed {
		t.Fatalf("matching-binding release = %#v, want a refusal", state)
	}
	if state.Safety != "blocked_release_not_applicable" {
		t.Fatalf("not-applicable safety = %s", state.Safety)
	}
	if state.NextAction == nil || state.NextAction.Code != "sync" {
		t.Fatalf("not-applicable next action = %#v", state.NextAction)
	}
	if f.custodyReturned() {
		t.Fatal("not-applicable release stamped custody")
	}
}

// TestReleaseNotAdvertisedWhenClosedPRRemoteStillMatchesBinding pins that a
// merged or closed PR whose remote branch still equals the binding never
// offers the release: that binding is not stale, release refuses it and
// points back at a re-check, so advertising it there would loop an agent
// between two refusing commands. The cached view sends the operator to the
// live retirement check; the live view confirms the intact binding and
// leaves the retired branch to a fresh run on a new PR.
func TestReleaseNotAdvertisedWhenClosedPRRemoteStillMatchesBinding(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)
	if err := f.db.UpdateRunPRState(f.run.ID, "closed"); err != nil {
		t.Fatal(err)
	}

	cached := f.service.InspectCached(f.ctx)
	if cached.State != StateClosed || cached.Safety != "blocked_closed" {
		t.Fatalf("cached classification = %#v", cached)
	}
	if cached.NextAction == nil || cached.NextAction.Code != "sync" {
		t.Fatalf("cached closed next action = %#v, want the live retirement check", cached.NextAction)
	}

	state := f.service.Refresh(f.ctx)
	if state.State != StateClosed || state.Safety != "blocked_closed" {
		t.Fatalf("live classification = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code == "release_binding" {
		t.Fatalf("intact-binding closed next action = %#v, must not advertise the release", state.NextAction)
	}
	if state.NextAction.Code != "run_pipeline" {
		t.Fatalf("intact-binding closed next action = %#v", state.NextAction)
	}
	if f.custodyReturned() {
		t.Fatal("no release was requested, but custody changed")
	}
}

// TestReleaseNotAdvertisedWhenMergedPRRemoteStillMatchesBinding is the
// merged twin: a retained remote that still equals the binding is not a
// stale binding, so neither view may send the operator into the release.
func TestReleaseNotAdvertisedWhenMergedPRRemoteStillMatchesBinding(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)
	if err := f.db.UpdateRunPRState(f.run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	cached := f.service.InspectCached(f.ctx)
	if cached.State != StateMergedRemoteRetained || cached.Safety != "blocked_merged" {
		t.Fatalf("cached classification = %#v", cached)
	}
	if cached.NextAction == nil || cached.NextAction.Code != "sync" {
		t.Fatalf("cached merged next action = %#v, want the live retirement check", cached.NextAction)
	}

	state := f.service.Refresh(f.ctx)
	if state.State != StateMergedRemoteRetained || state.Safety != "blocked_merged" {
		t.Fatalf("live classification = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code == "release_binding" {
		t.Fatalf("intact-binding merged next action = %#v, must not advertise the release", state.NextAction)
	}
	if state.NextAction.Code != "run_pipeline" {
		t.Fatalf("intact-binding merged next action = %#v", state.NextAction)
	}
}

// TestReleaseFailsClosedWhenGateHeadCannotBeAnchored pins the lossless
// contract: when the independently moved gate head cannot be anchored at the
// gate recovery ref (here a stale ref lock blocks the write), the release
// must refuse before re-pointing the gate branch instead of silently
// dropping the old gate head.
func TestReleaseFailsClosedWhenGateHeadCannotBeAnchored(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)
	rewriteRemoteForRelease(t, f)
	gateAnchor := custody.RecoveryGateRef(f.run.ID)
	lockPath := filepath.Join(f.gate, gateAnchor) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	state := f.service.Release(f.ctx)
	if state.Released || state.Changed {
		t.Fatalf("anchor-blocked release = %#v, want a refusal", state)
	}
	if state.Safety != "blocked_release_anchor_failed" {
		t.Fatalf("anchor-blocked safety = %s", state.Safety)
	}
	if f.custodyReturned() {
		t.Fatal("anchor-blocked release stamped custody")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("gate branch after refusal = %s, want the independently moved head %s untouched", got, f.preserved)
	}
}

// TestReleaseMissingRemoteBranchAlsoOffered pins the deleted-remote shape of
// defect 1: the binding is just as stale, and the blocked state still carries
// the release as its next_action.
func TestReleaseMissingRemoteBranchAlsoOffered(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	bindTerminalRun(t, f)
	mustRun(t, f.local, "push", f.remote, "--delete", "refs/heads/feature/recover")

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteMissing {
		t.Fatalf("missing-remote classification = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "release_binding" {
		t.Fatalf("missing-remote next action = %#v", state.NextAction)
	}

	released := f.service.Release(f.ctx)
	if !released.Released || !released.Changed {
		t.Fatalf("missing-remote release = %#v", released)
	}
	if !f.custodyReturned() {
		t.Fatal("missing-remote release did not stamp custody")
	}
}
