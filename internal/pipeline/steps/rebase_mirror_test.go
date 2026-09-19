package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRebaseStep_RefreshesGateMirrorAfterIntegration pins defect 4's second
// half (2026-09-17): the pipeline's own integration must move the gate mirror
// to the new head, so the mirror never lags the run's rebased lineage and
// publication's private-mirror guard cannot read that lag as content loss.
// The refresh is best-effort: a missing gate never fails the step.
func TestRebaseStep_RefreshesGateMirrorAfterIntegration(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, "")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, "", "init", "--bare", gateDir)
	// The mirror still holds the pre-integration head, exactly as it does
	// between a previous publication and this run's rebase.
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
	if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead, f.headSHA); err != nil {
		t.Fatalf("missing gate dir must be skipped, got: %v", err)
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

	newHead := gitCmd(t, f.dir, "rev-parse", "HEAD")

	t.Run("mirror ahead stays", func(t *testing.T) {
		gateDir := filepath.Join(t.TempDir(), "gate.git")
		gitCmd(t, "", "init", "--bare", gateDir)
		descendant := gitCmd(t, f.dir, "commit-tree", newHead+"^{tree}", "-p", newHead, "-m", "ahead")
		gitCmd(t, gateDir, "fetch", f.dir, descendant+":refs/heads/feature")
		sctx.GateDir = gateDir
		if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead, f.headSHA); err != nil {
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
		if err := refreshGateMirrorAfterIntegration(sctx.Ctx, sctx, newHead, f.headSHA); err == nil {
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
