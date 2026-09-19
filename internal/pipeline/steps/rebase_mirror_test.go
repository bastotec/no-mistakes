package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRebaseStep_RefreshesGateMirrorAfterIntegration pins defect 4's second
// half (2026-09-17): after a run has published, the pipeline's own next
// integration must move the gate mirror off the recorded publication and to
// the new head, so the mirror never lags the run's rebased lineage and
// publication's private-mirror guard cannot read that lag as content loss.
// The refresh is best-effort: a missing gate never fails the step, and an
// unpublished run's mirror (still the operator's submitted head) is custody
// territory and stays untouched until publication.
func TestRebaseStep_RefreshesGateMirrorAfterIntegration(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, "")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, "", "init", "--bare", gateDir)
	// The mirror still holds the head the run last published, exactly as it
	// does between a previous publication and this run's rebase.
	gitCmd(t, gateDir, "fetch", f.dir, f.headSHA+":refs/heads/feature")
	sctx.GateDir = gateDir
	sctx.LogFile = func(s string) { t.Log(s) }
	sctx.Run.LastPushedSHA = &f.headSHA

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean rebase, got approval gate: %s", outcome.Findings)
	}
	newHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if newHead == f.headSHA {
		t.Fatal("fixture did not move the head; test proves nothing")
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != newHead {
		t.Fatalf("gate mirror = %s after integration, want the integrated head %s", got, newHead)
	}

	// A missing gate directory is skipped silently: the refresh is
	// best-effort and must never fail an otherwise-complete integration.
	sctx.GateDir = filepath.Join(t.TempDir(), "absent.git")
	sctx.Run.HeadSHA = f.headSHA // simulate a second integration round
	if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead); err != nil {
		t.Fatalf("missing gate dir must be skipped, got: %v", err)
	}
}

// TestRebaseStep_UnpublishedRunKeepsGateMirrorAtSubmittedHead pins the
// custody half of the same contract: a run that has never published leaves
// the gate branch exactly where the operator pushed it, because until
// publication that branch IS the operator's branch mirror and custody
// (axi sync --recover) adopts the pipeline head from the run recovery ref
// instead. Publication's Decision 41-A exception already excuses a mirror at
// the submitted head, so refreshing it there guards nothing.
func TestRebaseStep_UnpublishedRunKeepsGateMirrorAtSubmittedHead(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, "")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, "", "init", "--bare", gateDir)
	// The mirror holds the submitted head of a run with no recorded
	// publication (Run.LastPushedSHA is nil).
	gitCmd(t, gateDir, "fetch", f.dir, f.headSHA+":refs/heads/feature")
	sctx.GateDir = gateDir
	sctx.LogFile = func(s string) { t.Log(s) }

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean rebase, got approval gate: %s", outcome.Findings)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got == f.headSHA {
		t.Fatal("fixture did not move the head; test proves nothing")
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != f.headSHA {
		t.Fatalf("unpublished gate mirror = %s after integration, want it left at the submitted head %s", got, f.headSHA)
	}
}

// TestRefreshGateMirrorAfterIntegrationNeverMovesWhatItCannotOwn pins the
// conservative ref discipline: a mirror already at or beyond the new head is
// left untouched, and a genuinely diverged mirror is left for publication's
// reconciliation machinery instead of being force-moved.
func TestRefreshGateMirrorAfterIntegrationNeverMovesWhatItCannotOwn(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, "")
	sctx.Run.LastPushedSHA = &f.headSHA

	newHead := gitCmd(t, f.dir, "rev-parse", "HEAD")

	t.Run("mirror ahead stays", func(t *testing.T) {
		gateDir := filepath.Join(t.TempDir(), "gate.git")
		gitCmd(t, "", "init", "--bare", gateDir)
		descendant := gitCmd(t, f.dir, "commit-tree", newHead+"^{tree}", "-p", newHead, "-m", "ahead")
		gitCmd(t, gateDir, "fetch", f.dir, descendant+":refs/heads/feature")
		sctx.GateDir = gateDir
		if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead); err != nil {
			t.Fatalf("refresh with a descendant mirror failed: %v", err)
		}
		if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != descendant {
			t.Fatalf("mirror moved to %s, want the preserved descendant %s", got, descendant)
		}
	})

	t.Run("diverged mirror is left alone", func(t *testing.T) {
		gateDir := filepath.Join(t.TempDir(), "gate.git")
		gitCmd(t, "", "init", "--bare", gateDir)
		// An unrelated root commit: diverged from both heads.
		other := t.TempDir()
		gitCmd(t, other, "init")
		gitCmd(t, other, "config", "user.name", "test")
		gitCmd(t, other, "config", "user.email", "test@test.com")
		if err := os.WriteFile(filepath.Join(other, "other.txt"), []byte("other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, other, "add", "-A")
		gitCmd(t, other, "commit", "-m", "unrelated")
		unrelated := gitCmd(t, other, "rev-parse", "HEAD")
		gitCmd(t, gateDir, "fetch", other, unrelated+":refs/heads/feature")
		sctx.GateDir = gateDir
		if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead); err == nil {
			t.Fatal("diverged mirror must be reported, not silently accepted")
		}
		if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != unrelated {
			t.Fatalf("diverged mirror moved to %s, want it left at %s", got, unrelated)
		}
	})
}

// TestRebaseStep_GateMirrorRefreshFailureDoesNotFailTheStep pins the
// best-effort contract end to end: when the gate exists but cannot be
// refreshed, the integration still completes with a logged warning.
func TestRebaseStep_GateMirrorRefreshFailureDoesNotFailTheStep(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, config.RebaseStrategyMerge)

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	// Not a bare repository: validation fails, the refresh warns, the step
	// still completes.
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sctx.GateDir = gateDir

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("integration failed on an unusable gate mirror: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean merge, got approval gate: %s", outcome.Findings)
	}
}
