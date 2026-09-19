package branchsync

import (
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
