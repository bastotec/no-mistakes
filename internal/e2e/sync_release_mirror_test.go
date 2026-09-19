//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The four daemon/sync/custody defects fixed on this branch (2026-09-17) are
// driven here end to end against the real binary, real daemon, real gate, and
// real git remotes, exactly the way an end user hits them:
//
//  1. a terminal run's stale push binding strands the branch after an
//     out-of-band remote rewrite, and `axi sync --release` is the sanctioned
//     exit (plus its fail-closed preconditions);
//  2. a fresh trigger never adopts a terminal run - a fresh `axi run` on a
//     head with a terminal run drives a NEW run instead of replaying dead
//     records;
//  3. custody no longer declares unpublished commits unrecoverable while they
//     are intact in the daemon's private mirror: the mirror-backed keep-local
//     return is offered and works;
//  4. an ordinary in-pipeline rebase (base advanced, context drifted) no
//     longer trips the private-mirror "at-risk" refusal: the run publishes.

// rewriteUpstreamBranchForce pushes main over the branch with --force,
// simulating the out-of-band rewrite that strands a terminal run's push
// binding: afterwards the live remote head is an ancestor of the pushed
// binding, so the binding can never be re-matched by an ordinary push.
func rewriteUpstreamBranchForce(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "--force", "origin", "main:refs/heads/"+branch); err != nil {
		t.Fatalf("out-of-band rewrite of origin/%s: %v\n%s", branch, err, out)
	}
	return h.UpstreamBranchSHA(branch)
}

// TestSyncReleaseStaleBindingJourney is the defect-1 end-user journey: a run
// completes and binds the push; someone rewrites the remote out of band; sync
// --check reports the stale binding with the release_binding next action;
// sync --release anchors the pipeline head, re-points the gate branch, stamps
// custody, and never touches the rewritten remote; a fresh run then starts.
// It also pins the fail-closed precondition (an intact binding is refused)
// and the defect-2-adjacent contract that a fresh trigger on a head with a
// terminal run drives a NEW run rather than replaying its records.
func TestSyncReleaseStaleBindingJourney(t *testing.T) {
	// branchSyncScenario makes the review step return one auto-fix finding,
	// so the pipeline publishes a fix commit the operator's branch has never
	// seen: after release that head must be anchored rather than merely
	// reachable, which is the lossless half of the contract.
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-release", "seed.txt", "seed\n", "seed release init")
	initWorktree := h.AddWorktree("init-release")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/release-journey"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	runOut, err := h.RunInDir(operator, "axi", "run", "--yes", "--intent", "ship the feature and bind the push")
	if err != nil || !strings.Contains(runOut, "outcome: passed") {
		t.Fatalf("initial run did not pass: %v\n%s", err, runOut)
	}
	run := h.WaitForRun(branch, 10*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error=%v", run.Status, deref(run.Error))
	}
	pushed := h.UpstreamBranchSHA(branch)
	if pushed != run.HeadSHA {
		t.Fatalf("upstream %s != run head %s", pushed, run.HeadSHA)
	}
	if pushed == submitted {
		t.Fatal("test setup: the pipeline published the submitted head unchanged; no pipeline-only commit exists to anchor")
	}
	t.Logf("run %s passed; submitted=%s pushed binding=%s", run.ID, submitted, pushed)

	// Adversarial precondition: while the live remote still EQUALS the
	// binding, release must refuse - ordinary synchronization applies and a
	// release would discard a valid binding for nothing.
	refuseOut, refuseErr := h.RunInDir(operator, "axi", "sync", "--release")
	if refuseErr == nil {
		t.Fatalf("release on an intact binding should exit non-zero:\n%s", refuseOut)
	}
	for _, want := range []string{"blocked_release_not_applicable", "command: no-mistakes axi sync --check"} {
		if !strings.Contains(refuseOut, want) {
			t.Errorf("intact-binding release refusal missing %q:\n%s", want, refuseOut)
		}
	}
	t.Logf("intact-binding release refused:\n%s", refuseOut)

	// The out-of-band rewrite that strands the binding for good.
	mainHead := strings.TrimSpace(h.WorktreeRefSHA("main"))
	rewritten := rewriteUpstreamBranchForce(t, h, branch)
	if rewritten == pushed || rewritten != mainHead {
		t.Fatalf("remote rewrite landed at %s (main=%s, binding=%s)", rewritten, mainHead, pushed)
	}

	checkOut, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
	if checkErr == nil {
		t.Fatalf("stranded sync --check should exit non-zero:\n%s", checkOut)
	}
	for _, want := range []string{
		"state: remote_rewritten",
		"safety: blocked_remote_rewritten",
		"code: release_binding",
		"command: no-mistakes axi sync --release",
		"release this terminal run's stale binding",
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("stranded check missing %q:\n%s", want, checkOut)
		}
	}
	t.Logf("stranded end-user report:\n%s", checkOut)

	// Second adversarial angle: a release attempt while offline-ish inputs
	// are fine but the run is active is unit-pinned; here the human `sync`
	// surface must also refuse without --yes on a non-interactive terminal.
	humanRefuseOut, humanRefuseErr := h.RunInDir(operator, "sync", "--release")
	if humanRefuseErr == nil {
		t.Fatalf("human sync --release without --yes on a non-tty should exit non-zero:\n%s", humanRefuseOut)
	}
	if !strings.Contains(humanRefuseOut, "sync --release --yes") {
		t.Errorf("human non-interactive refusal should point at --yes:\n%s", humanRefuseOut)
	}

	releaseOut, releaseErr := h.RunInDir(operator, "axi", "sync", "--release")
	if releaseErr != nil {
		t.Fatalf("axi sync --release: %v\n%s", releaseErr, releaseOut)
	}
	for _, want := range []string{"released: true", "binding_released", "custody returned", "code: run_pipeline", "anchored at the recovery refs"} {
		if !strings.Contains(releaseOut, want) {
			t.Errorf("release output missing %q:\n%s", want, releaseOut)
		}
	}
	t.Logf("release output:\n%s", releaseOut)

	// The release is lossless and never rewrites the changed remote: the
	// pipeline head stays anchored at the run recovery ref, the gate branch
	// follows the operator's head, and origin still holds the rewrite.
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if got, err := h.runGit(context.Background(), gateDir, "rev-parse", custody.RecoveryRef(run.ID)); err != nil || strings.TrimSpace(string(got)) != run.HeadSHA {
		t.Fatalf("recovery ref = %s (err %v), want anchored pipeline head %s", strings.TrimSpace(string(got)), err, run.HeadSHA)
	}
	if got, err := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch); err != nil || strings.TrimSpace(string(got)) != submitted {
		t.Fatalf("gate branch after release = %s (err %v), want operator head %s", strings.TrimSpace(string(got)), err, submitted)
	}
	if got := h.UpstreamBranchSHA(branch); got != rewritten {
		t.Fatalf("release rewrote the changed remote: origin=%s, want the out-of-band rewrite %s untouched", got, rewritten)
	}

	// The branch is no longer stranded: a fresh axi run on the same head must
	// drive a NEW run (defect 2's never-attach contract for the reachable
	// case) and publish again, fast-forwarding over the rewound remote.
	runsBefore := len(h.Runs())
	freshOut, freshErr := h.RunInDir(operator, "axi", "run", "--yes", "--intent", "revalidate after releasing the stale binding")
	if freshErr != nil || !strings.Contains(freshOut, "outcome: passed") {
		t.Fatalf("fresh run after release did not pass: %v\n%s", freshErr, freshOut)
	}
	runs := h.Runs()
	if len(runs) != runsBefore+1 {
		t.Fatalf("fresh run count = %d, want %d (a replacement run, not a replay)", len(runs), runsBefore+1)
	}
	var secondID string
	for i := range runs {
		if runs[i].ID != run.ID {
			secondID = runs[i].ID
		}
	}
	if secondID == "" {
		t.Fatalf("no replacement run found after release; runs=%v", runs)
	}
	final := h.WaitForRun(branch, 90*time.Second)
	if final.ID != secondID {
		t.Fatalf("fresh axi run presented terminal run %s instead of the replacement %s", final.ID, secondID)
	}
	if final.Status != types.RunCompleted {
		t.Fatalf("replacement run status = %s, error=%v", final.Status, deref(final.Error))
	}
	if got := h.UpstreamBranchSHA(branch); got != final.HeadSHA {
		t.Fatalf("origin after replacement run = %s, want run head %s", got, final.HeadSHA)
	}
	t.Logf("replacement run %s re-validated and published %s", final.ID, final.HeadSHA)
}

// TestSyncMirrorKeepLocalJourney is the defect-3 end-user journey: a run is
// cancelled with unpublished pipeline commits that live only in the daemon's
// private mirror, and the operator has since committed new work the gate has
// never seen. Pre-fix this state reported the work as unrecoverable; now
// sync --check offers the mirror-backed keep-local custody return and
// sync --recover --keep-local performs it without touching the worktree.
func TestSyncMirrorKeepLocalJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-mirror", "seed.txt", "seed\n", "seed mirror init")
	initWorktree := h.AddWorktree("init-mirror")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/mirror-keep-local"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before the daemon dies")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if _, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v", err)
	}
	abortOut, abortErr := h.RunInDir(operator, "axi", "abort")
	if abortErr != nil {
		t.Fatalf("axi abort: %v\n%s", abortErr, abortOut)
	}
	run := h.WaitForRun(branch, 30*time.Second)
	if run.Status != types.RunCancelled {
		t.Fatalf("run status after abort = %s", run.Status)
	}
	if run.HeadSHA == submitted {
		t.Fatal("pipeline fix commit was not created before the abort")
	}
	t.Logf("run %s cancelled with unpublished head %s (submitted %s)", run.ID, run.HeadSHA, submitted)

	// The operator keeps working: a new commit the gate has never seen makes
	// the ordinary adoption proof impossible while the mirror still holds the
	// preserved pipeline head intact.
	if err := os.WriteFile(filepath.Join(operator, "operator-work.txt"), []byte("later operator work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), operator, "add", "operator-work.txt"); err != nil {
		t.Fatalf("stage operator work: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), operator, "commit", "-m", "later operator work"); err != nil {
		t.Fatalf("commit operator work: %v\n%s", err, out)
	}
	localHead := strings.TrimSpace(h.WorktreeRefSHA(branch))
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if _, err := h.runGit(context.Background(), gateDir, "cat-file", "-e", localHead+"^{commit}"); err == nil {
		t.Fatalf("test setup: the gate unexpectedly already has the operator head %s", localHead)
	}

	// Defect 3: the commits preserved in the daemon's mirror are NOT
	// unrecoverable. The check must offer the mirror-backed keep-local return.
	checkOut, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
	if checkErr == nil {
		t.Fatalf("mirror-stranded sync --check should exit non-zero:\n%s", checkOut)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"safety: blocked_pipeline_owned_recoverable",
		"source: gate_mirror",
		"keep_local: true",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover --keep-local",
		"daemon's private mirror",
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("mirror-stranded check missing %q:\n%s", want, checkOut)
		}
	}
	for _, forbidden := range []string{"cannot be used safely", "blocked_recover_manual_reconciliation"} {
		if strings.Contains(checkOut, forbidden) {
			t.Errorf("mirror-stranded check still declares the work unrecoverable (%q):\n%s", forbidden, checkOut)
		}
	}
	t.Logf("mirror-backed recovery offer:\n%s", checkOut)

	// Adversarial boundary from the deliberate decisions: once the invoking
	// worktree has SEEN the preserved head (operator fetched it to inspect),
	// the mirror-backed keep-local offer must withdraw - a worktree holding
	// both divergent heads keeps manual reconciliation as its exit.
	gateFetchOut, gateFetchErr := h.runGit(context.Background(), operator, "fetch", "--no-tags", gateDir, run.HeadSHA)
	if gateFetchErr != nil {
		t.Fatalf("fetch preserved head for inspection: %v\n%s", gateFetchErr, gateFetchOut)
	}
	seenOut, seenErr := h.RunInDir(operator, "axi", "sync", "--check")
	if seenErr == nil {
		t.Fatalf("check with both heads in the worktree should stay blocked:\n%s", seenOut)
	}
	for _, want := range []string{"blocked_recover_manual_reconciliation", "code: inspect_and_reconcile_manually"} {
		if !strings.Contains(seenOut, want) {
			t.Errorf("both-heads-in-worktree check missing %q:\n%s", want, seenOut)
		}
	}
	if strings.Contains(seenOut, "source: gate_mirror") {
		t.Errorf("keep-local offer survived the worktree seeing the preserved head:\n%s", seenOut)
	}
	t.Logf("boundary: worktree holding both divergent heads keeps manual reconciliation:\n%s", seenOut)

	recoverOut, recoverErr := h.RunInDir(operator, "axi", "sync", "--recover", "--keep-local")
	if recoverErr != nil {
		t.Fatalf("mirror-backed keep-local recovery: %v\n%s", recoverErr, recoverOut)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned"} {
		if !strings.Contains(recoverOut, want) {
			t.Errorf("keep-local recovery output missing %q:\n%s", want, recoverOut)
		}
	}
	t.Logf("keep-local recovery output:\n%s", recoverOut)

	// Keep-local contract: the worktree keeps the operator's head, the gate
	// branch follows it, and the preserved pipeline head stays anchored.
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != localHead {
		t.Fatalf("keep-local moved the worktree: %s != %s", got, localHead)
	}
	if got, err := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch); err != nil || strings.TrimSpace(string(got)) != localHead {
		t.Fatalf("gate branch after keep-local = %s (err %v), want %s", strings.TrimSpace(string(got)), err, localHead)
	}
	if got, err := h.runGit(context.Background(), gateDir, "rev-parse", custody.RecoveryRef(run.ID)); err != nil || strings.TrimSpace(string(got)) != run.HeadSHA {
		t.Fatalf("preserved head anchor = %s (err %v), want %s", strings.TrimSpace(string(got)), err, run.HeadSHA)
	}

	// The branch is usable again: a fresh run starts, belongs to a NEW run
	// (not the cancelled one), and drives to the review gate.
	runsBefore := len(h.Runs())
	freshOut, freshErr := h.RunInDir(operator, "axi", "run", "--intent", "resume work after the mirror-backed custody return")
	if freshErr != nil {
		t.Fatalf("fresh run after keep-local recovery blocked: %v\n%s", freshErr, freshOut)
	}
	if !strings.Contains(freshOut, "awaiting_approval") {
		t.Errorf("fresh run did not reach the review gate:\n%s", freshOut)
	}
	if got := len(h.Runs()); got != runsBefore+1 {
		t.Fatalf("fresh run count = %d, want %d", got, runsBefore+1)
	}
	active := h.ActiveRun(branch)
	if active == nil || active.ID == run.ID || active.Status != types.RunRunning {
		t.Fatalf("fresh run after recovery: active run = %#v, want a new running run distinct from %s", active, run.ID)
	}
	t.Logf("fresh run after mirror-backed recovery:\n%s", freshOut)
}

// TestRebaseStaleMirrorJourney is the defect-4 end-user journey: the default
// branch advances under a gated branch with a change adjacent to the branch's
// hunk, so the pipeline's own rebase replays the change cleanly while its
// patch identity drifts (moved context lines). Pre-fix, publication's private
// mirror guard read the stale mirror plus the drifted patch ids as content
// loss and failed the run with "at-risk commit(s) contain content absent
// from live head"; now the run publishes.
func TestRebaseStaleMirrorJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.CommitChange("init-stale-mirror", "seed.txt", "seed\n", "seed stale mirror init")
	initWorktree := h.AddWorktree("init-stale-mirror")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/stale-mirror"
	// A ten-line base file on main; the branch edits line 5; main then edits
	// line 3 - two lines above the branch's change, inside its hunk context
	// (context spans three lines) but not touching it, so the replay is clean
	// while the commit's patch id cannot stay stable across the rebase (the
	// context hash drifts).
	base := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	if out, err := h.runGit(context.Background(), h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v\n%s", err, out)
	}
	h.CommitChange("main", "numbered.txt", base, "add numbered base file")
	submitted := h.CommitChange(branch, "numbered.txt", strings.Replace(base, "5\n", "5-feature\n", 1), "edit line five on the branch")
	advanced := advanceMain(t, h, "numbered.txt", strings.Replace(base, "3\n", "3-main\n", 1), "edit line three on main")
	h.Checkout(branch)
	if advanced == submitted {
		t.Fatal("test setup: main advance collided with the branch head")
	}
	operator := h.AddWorktree(branch)
	h.PushToGate(branch)

	run := h.waitForRunStatus(branch, 120*time.Second, func(status types.RunStatus) bool { return status.Terminal() }, "terminal")
	if run == nil || !run.Status.Terminal() {
		if run != nil {
			for _, s := range run.Steps {
				t.Logf("step %s: %s", s.StepName, s.Status)
			}
		}
		t.Fatalf("ordinary rebase run did not finish (last status=%v)", run)
	}
	if run.HeadSHA == submitted {
		t.Fatalf("run published the submitted head %s unchanged; the base advance did not integrate", submitted)
	}

	// The routine rebase must NOT be reported as at-risk: the run completed
	// and published, and nothing in its step logs mentions the refusal.
	for _, step := range []string{"rebase", "push"} {
		logsOut, logsErr := h.RunInDir(operator, "axi", "logs", "--run", run.ID, "--step", step)
		if logsErr != nil {
			t.Fatalf("axi logs --step %s: %v\n%s", step, logsErr, logsOut)
		}
		if strings.Contains(logsOut, "at-risk") {
			t.Fatalf("routine rebase reported at-risk content in %s step:\n%s", step, logsOut)
		}
		t.Logf("%s step log:\n%s", step, logsOut)
	}

	// The published head descends from the advanced base and the gate mirror
	// tracks the run's rebased lineage.
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if _, err := h.runGit(context.Background(), gateDir, "merge-base", "--is-ancestor", advanced, run.HeadSHA); err != nil {
		t.Fatalf("published head %s does not integrate the advanced base %s", run.HeadSHA, advanced)
	}
	if got := h.UpstreamBranchSHA(branch); got != run.HeadSHA {
		t.Fatalf("origin/%s = %s, want published run head %s", branch, got, run.HeadSHA)
	}
	if got, err := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch); err != nil || strings.TrimSpace(string(got)) != run.HeadSHA {
		t.Fatalf("gate mirror = %s (err %v), want run head %s", strings.TrimSpace(string(got)), err, run.HeadSHA)
	}
	t.Logf("ordinary rebase published %s (submitted %s, base advanced to %s) with no at-risk refusal", run.HeadSHA, submitted, advanced)

	// The other half of the defect: the OPERATOR's own ordinary rebase. The
	// gate mirror holds the published head; the operator fast-forwards to it,
	// advances main again next to the branch's hunk, rebases locally (clean
	// replay, drifted patch identity), and pushes a fresh run. Pre-fix this
	// exact routine flow was refused with "at-risk commit(s) contain content
	// absent from live head"; now the stale mirror is recognized as merely
	// rebased (whole-tree survival) and the fresh run starts and publishes.
	if out, err := h.RunInDir(operator, "axi", "sync"); err != nil {
		t.Fatalf("operator sync to published head: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != run.HeadSHA {
		t.Fatalf("operator head after sync = %s, want published %s", got, run.HeadSHA)
	}
	advanced2 := advanceMain(t, h, "numbered.txt", strings.Replace(strings.Replace(base, "3\n", "3-main\n", 1), "7\n", "7-main\n", 1), "edit lines three and seven on main")
	if out, err := h.runGit(context.Background(), operator, "fetch", "origin", "main"); err != nil {
		t.Fatalf("operator fetch origin/main: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), operator, "rebase", "origin/main"); err != nil {
		t.Fatalf("operator local rebase (expected clean): %v\n%s", err, out)
	}
	localRebased := strings.TrimSpace(h.WorktreeRefSHA(branch))
	if localRebased == run.HeadSHA {
		t.Fatal("test setup: local rebase produced the same head; lineage was not rewritten")
	}
	runsBefore := len(h.Runs())
	repushOut, repushErr := h.RunInDir(operator, "axi", "run", "--yes", "--intent", "publish the locally rebased branch")
	if repushErr != nil || !strings.Contains(repushOut, "outcome: passed") {
		t.Fatalf("fresh run after the operator's ordinary local rebase did not pass: %v\n%s", repushErr, repushOut)
	}
	for _, forbidden := range []string{"at-risk", "refusing to reconcile"} {
		if strings.Contains(repushOut, forbidden) {
			t.Fatalf("ordinary local rebase still refused (%q):\n%s", forbidden, repushOut)
		}
	}
	runs := h.Runs()
	if len(runs) != runsBefore+1 {
		t.Fatalf("run count after local-rebase repush = %d, want %d", len(runs), runsBefore+1)
	}
	second := h.WaitForRun(branch, 90*time.Second)
	if second.Status != types.RunCompleted {
		t.Fatalf("local-rebase republish run status = %s, error=%v", second.Status, deref(second.Error))
	}
	if got := h.UpstreamBranchSHA(branch); got != second.HeadSHA {
		t.Fatalf("origin after local-rebase republish = %s, want run head %s", got, second.HeadSHA)
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "merge-base", "--is-ancestor", advanced2, second.HeadSHA); err != nil {
		t.Fatalf("republished head %s does not carry the second base advance %s", second.HeadSHA, advanced2)
	}
	t.Logf("operator's ordinary local rebase republished %s (was %s) with no at-risk refusal", second.HeadSHA, run.HeadSHA)
}
