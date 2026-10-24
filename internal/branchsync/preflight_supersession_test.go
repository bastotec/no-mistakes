package branchsync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"

	_ "modernc.org/sqlite"
)

// preflightSupersessionFixture models the fleet-dream incident shape with real
// temp Git repos and a real SQLite database: an older run that failed inside
// launch's explicit-PR preflight (never started: no step, no invocation, no
// gates, no publication, head never moved off the submitted head, no terminal
// verification - exactly as UpdateRunError leaves it), and a newer terminal run
// that published a divergent final head (authorized fixes/rebase changed
// content, so the final head is NOT a descendant of the submitted head) with an
// exact repository/branch push binding while the bare gate branch sits at that
// pushed head and the invoking worktree stays clean at the submitted head.
type preflightSupersessionFixture struct {
	t          *testing.T
	ctx        context.Context
	database   *db.DB
	repo       *db.Repo
	service    *Service
	local      string
	gate       string
	remote     string
	dbPath     string
	base       string
	submitted  string
	pushed     string
	olderID    string
	newerID    string
	remoteName string
}

// newPreflightSupersessionFixture builds the incident shape. The remote named
// f.remoteName receives the newer run's pushed head so live-remote equality
// checks see a matching branch, and the invoking worktree has that remote
// configured.
func newPreflightSupersessionFixture(t *testing.T) *preflightSupersessionFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	// The submitted head: the operator's own change, later terminal-published
	// under rewritten SHAs by the newer run.
	mustRun(t, local, "checkout", "-b", "feature/preflight")
	mustWrite(t, filepath.Join(local, "file.txt"), "base\nsubmitted change\n")
	mustRun(t, local, "commit", "-am", "submitted change")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The gate received the submitted head first (the older attempt's launch
	// base), then the newer run published a divergent final head onto it: an
	// authorized fix rewrites the same lines, so the published head neither
	// contains nor replays the submitted commit exactly.
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main")
	mustRun(t, local, "push", gate, submitted+":refs/heads/feature/preflight")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/preflight")
	// The pipeline's own commits on top of the submitted head: the authorized
	// fix the newer run's validation produced.
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "authorized fix\n")
	mustRun(t, pipeline, "add", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	// Rebuild the final head as a divergent replay: the submitted change and
	// the authorized fix are replayed under new SHAs onto an advanced base, so
	// the published head carries the submitted work WITHOUT containing the
	// submitted commit - the exact authorized-fix/rebase divergence the
	// incident showed (final head NOT a descendant of the submitted head).
	mustRun(t, pipeline, "checkout", "-b", "published", base)
	mustWrite(t, filepath.Join(pipeline, "advance.txt"), "base advanced\n")
	mustRun(t, pipeline, "add", "advance.txt")
	mustRun(t, pipeline, "commit", "-m", "base advanced")
	mustRun(t, pipeline, "cherry-pick", "feature/preflight~1")
	mustRun(t, pipeline, "cherry-pick", "feature/preflight")
	pushed := mustRun(t, pipeline, "rev-parse", "HEAD")

	mustRun(t, pipeline, "push", gate, "+HEAD:refs/heads/feature/preflight")
	mustRun(t, pipeline, "push", remote, "HEAD:refs/heads/feature/preflight")
	mustRun(t, local, "remote", "add", "origin", remote)
	mustRun(t, local, "fetch", "origin", "feature/preflight")
	// Keep the operator clean at the submitted head: the invoking worktree is
	// the caller the incident described, untouched by the newer run's work.
	if head := mustRun(t, local, "rev-parse", "HEAD"); head != submitted {
		t.Fatalf("fixture: operator head moved to %s", head)
	}
	out := mustRun(t, local, "status", "--porcelain")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("fixture: operator worktree dirty: %q", out)
	}

	dbPath := filepath.Join(root, "state.sqlite")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}

	// The older preflight attempt: newest-first ordering needs it created
	// strictly before the newer run. Launch records the worktree placement
	// before explicit-PR validation and its setup-failure defer removes the
	// directory while the column survives, so the fixture models exactly that:
	// a recorded placement whose directory no longer exists.
	older, err := database.InsertRun(repo.ID, "feature/preflight", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	ghostWorktree := filepath.Join(root, "removed-worktree")
	if err := os.MkdirAll(ghostWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunWorktreeDir(older.ID, ghostWorktree); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ghostWorktree); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunError(older.ID, fmt.Sprintf(
		"validate existing PR: explicit PR source repository/ref does not match the configured push target and run branch; feature/preflight publishes to https://github.com/example/example/pull/7 - if that is no longer the right target, run `no-mistakes axi run --retire-existing-pr` on the branch to return to ordinary discovery")); err != nil {
		t.Fatal(err)
	}

	// Strict ordering: ULID seconds differ, and even identical-second ULIDs
	// order lexically by randomness, so a real sleep is the only guaranteed
	// strict older/newer pair here.
	time.Sleep(1100 * time.Millisecond)

	// The newer authoritative run: same submitted head, terminal, verified at
	// the published head with an exact push binding.
	newer, err := database.InsertRun(repo.ID, "feature/preflight", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(newer.ID, db.PushBinding{
		HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(remote), Ref: "refs/heads/feature/preflight",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatusWithVerifiedHead(newer.ID, "ci failed", types.RunFailed, pushed); err != nil {
		t.Fatal(err)
	}

	service := &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate}
	return &preflightSupersessionFixture{
		t: t, ctx: ctx, database: database, repo: repo, service: service,
		local: local, gate: gate, remote: remote, dbPath: dbPath,
		base: base, submitted: submitted, pushed: pushed,
		olderID: older.ID, newerID: newer.ID, remoteName: "origin",
	}
}

// snapshot captures every fact the read-only proofs must leave untouched: both
// run rows verbatim, the gate branch, the remote branch, and the local branch.
type preflightSnapshot struct {
	older   *db.Run
	newer   *db.Run
	gate    string
	remote  string
	local   string
	anchors map[string]string
}

func (f *preflightSupersessionFixture) snapshot() preflightSnapshot {
	f.t.Helper()
	older, err := f.database.GetRun(f.olderID)
	if err != nil || older == nil {
		f.t.Fatalf("reload older run: %v", err)
	}
	newer, err := f.database.GetRun(f.newerID)
	if err != nil || newer == nil {
		f.t.Fatalf("reload newer run: %v", err)
	}
	anchors := map[string]string{}
	for _, runID := range []string{f.olderID, f.newerID} {
		for _, dir := range []string{f.local, f.gate} {
			ref := custody.RecoveryRef(runID)
			if target, exists, err := mustRefTarget(f.t, f.ctx, dir, ref); err == nil && exists {
				anchors[dir+" "+ref] = target
			}
		}
	}
	return preflightSnapshot{
		older: older, newer: newer,
		gate:    mustRun(f.t, f.gate, "rev-parse", "refs/heads/feature/preflight"),
		remote:  mustRun(f.t, f.remote, "rev-parse", "refs/heads/feature/preflight"),
		local:   mustRun(f.t, f.local, "rev-parse", "refs/heads/feature/preflight"),
		anchors: anchors,
	}
}

func mustRefTarget(t *testing.T, ctx context.Context, dir, ref string) (string, bool, error) {
	target, exists, err := gitpkg.ExactRefTarget(ctx, dir, ref)
	if err != nil {
		t.Fatalf("inspect ref %s in %s: %v", ref, dir, err)
	}
	return target, exists, nil
}

func (f *preflightSupersessionFixture) assertUnchanged(before preflightSnapshot, where string) {
	f.t.Helper()
	after := f.snapshot()
	if !reflect.DeepEqual(after.older, before.older) {
		f.t.Fatalf("%s: older run row mutated:\n%+v\n->\n%+v", where, before.older, after.older)
	}
	if !reflect.DeepEqual(after.newer, before.newer) {
		f.t.Fatalf("%s: newer run row mutated:\n%+v\n->\n%+v", where, before.newer, after.newer)
	}
	if after.gate != before.gate || after.remote != before.remote || after.local != before.local {
		f.t.Fatalf("%s: refs moved: gate %s->%s remote %s->%s local %s->%s", where,
			before.gate, after.gate, before.remote, after.remote, before.local, after.local)
	}
	if len(after.anchors) != len(before.anchors) {
		f.t.Fatalf("%s: recovery anchors appeared or disappeared: %v -> %v", where, before.anchors, after.anchors)
	}
	for ref, target := range before.anchors {
		if after.anchors[ref] != target {
			f.t.Fatalf("%s: recovery anchor %s moved %s -> %s", where, ref, target, after.anchors[ref])
		}
	}
}

// assertNewerAuthoritative pins the fixed selection: inspect selects the newer
// run, never the older attempt, and classifies the newer run's own relation to
// the caller (here: behind the published head, the ordinary guarded-sync
// verdict) rather than any old-attempt recovery offer.
func (f *preflightSupersessionFixture) assertNewerAuthoritative(where string) State {
	f.t.Helper()
	state := f.service.InspectCached(f.ctx)
	if state.Pipeline.RunID != f.newerID {
		f.t.Fatalf("%s: selection kept the obsolete preflight attempt: run=%s (want newer %s) state=%s safety=%s err=%v",
			where, state.Pipeline.RunID, f.newerID, state.State, state.Safety, state.Error)
	}
	if state.Pipeline.RunID == f.olderID || strings.Contains(fmt.Sprint(state.Error), f.olderID) {
		f.t.Fatalf("%s: state still describes the old attempt: %#v", where, state)
	}
	return state
}

// TestPreflightSupersessionSelectsNewerAuthoritativeRun is the incident's
// positive case: the proven never-started preflight attempt no longer steals
// custody selection from the newer authoritative publication, through the
// real InspectCached / Refresh / Recover surface, with both run rows and every
// relevant ref snapshotted before and after the read-only selection.
func TestPreflightSupersessionSelectsNewerAuthoritativeRun(t *testing.T) {
	t.Parallel()

	f := newPreflightSupersessionFixture(t)
	before := f.snapshot()

	// The newer run is selected and the caller's GENUINE divergence from it
	// (the caller is still at the submitted head; the newer run published a
	// replayed, rewritten final head) is reported separately - never silently
	// resolved by the selection fix, exactly as the design requires.
	state := f.assertNewerAuthoritative("inspect")
	if state.State != StateDiverged || (state.Safety != "blocked_diverged" && state.Safety != "refresh_required") {
		t.Fatalf("newer authoritative run classified as %s (%s), want the separately reported diverged relation: err=%v", state.State, state.Safety, state.Error)
	}
	f.assertUnchanged(before, "inspect")

	// Refresh observes the live remote and keeps reporting the newer run's own
	// facts - the normal sync relation, never the old attempt's recovery offer.
	refreshed := f.service.Refresh(f.ctx)
	if refreshed.Pipeline.RunID != f.newerID {
		t.Fatalf("refresh selection = %s, want newer %s", refreshed.Pipeline.RunID, f.newerID)
	}
	if refreshed.State != StateDiverged && refreshed.Safety != "refresh_required" {
		t.Fatalf("refresh classified %s (%s) against the newer run: err=%v", refreshed.State, refreshed.Safety, refreshed.Error)
	}
	f.assertUnchanged(before, "refresh")

	// Recover has nothing to recover: the selected newer run published its
	// head, so recovery must refuse without touching the older attempt's
	// history, bindings, or the newer run's certification.
	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered {
		t.Fatalf("recover unexpectedly returned custody: %#v", recovered)
	}
	f.assertUnchanged(before, "recover")
}

// TestPreflightSupersessionSurvivesDBReopen repeats the authoritative
// selection after closing and reopening the database: the verdict is derived
// from durable records, and all old history and bindings are unchanged.
func TestPreflightSupersessionSurvivesDBReopen(t *testing.T) {
	t.Parallel()

	f := newPreflightSupersessionFixture(t)
	before := f.snapshot()
	f.assertNewerAuthoritative("before reopen")

	if err := f.database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.database = reopened
	f.service.DB = reopened
	repo, err := reopened.GetRepo(f.repo.ID)
	if err != nil || repo == nil {
		t.Fatalf("reload repo: %v", err)
	}
	f.repo = repo
	f.service.Repo = repo

	f.assertNewerAuthoritative("after reopen")
	f.assertUnchanged(before, "after reopen")
}

func TestPreflightSupersessionSurvivesSubsequentRerunAndMissingHeadRecovery(t *testing.T) {
	t.Parallel()

	f := newPreflightSupersessionFixture(t)
	time.Sleep(1100 * time.Millisecond)
	repeated, err := f.database.InsertRun(f.repo.ID, "feature/preflight", f.pushed, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunPushBinding(repeated.ID, db.PushBinding{
		HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/preflight",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunStatusWithVerifiedHead(repeated.ID, types.RunCompleted, f.pushed); err != nil {
		t.Fatal(err)
	}
	repeatedBefore, err := f.database.GetRun(repeated.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	state := f.service.InspectCached(f.ctx)
	if state.Pipeline.RunID != repeated.ID || state.State != StateDiverged || state.Safety != "refresh_required" {
		t.Fatalf("subsequent rerun selection = %#v", state)
	}
	refreshed := f.service.Refresh(f.ctx)
	if refreshed.Pipeline.RunID != repeated.ID || refreshed.State != StateDiverged || refreshed.Safety != SafetySafeEquivalentAdvance {
		t.Fatalf("subsequent rerun refresh = %#v", refreshed)
	}
	f.assertUnchanged(before, "subsequent rerun")

	time.Sleep(1100 * time.Millisecond)
	missing, err := f.database.InsertRun(f.repo.ID, "feature/preflight", f.pushed, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunStatusWithVerifiedHead(missing.ID, types.RunFailed, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}
	state = f.service.InspectCached(f.ctx)
	if state.Pipeline.RunID != missing.ID || state.Safety != "blocked_recover_preserved_head_missing" ||
		state.NextAction == nil || state.NextAction.Code != "recover_custody" || state.NextAction.Command != "no-mistakes axi sync --recover --keep-local" {
		t.Fatalf("missing-head selection after subsequent rerun = %#v", state)
	}
	f.assertUnchanged(before, "missing-head inspection")
	kept := f.service.Recover(f.ctx, true)
	if !kept.Recovered {
		t.Fatalf("missing-head recovery after subsequent rerun = %#v", kept)
	}
	after := f.snapshot()
	if !reflect.DeepEqual(after.older, before.older) || !reflect.DeepEqual(after.newer, before.newer) {
		t.Fatal("missing-head recovery mutated superseded or published history")
	}
	repeatedAfter, err := f.database.GetRun(repeated.ID)
	if err != nil || !reflect.DeepEqual(repeatedAfter, repeatedBefore) {
		t.Fatalf("subsequent published run mutated: %#v, %v", repeatedAfter, err)
	}
	missingAfter, err := f.database.GetRun(missing.ID)
	if err != nil || missingAfter == nil || missingAfter.CustodyReturnedAt == nil {
		t.Fatalf("missing run custody not returned: %#v, %v", missingAfter, err)
	}
}

func TestPreflightSupersessionReportsChangedLiveRemote(t *testing.T) {
	t.Parallel()

	f := newPreflightSupersessionFixture(t)
	mustRun(t, f.local, "push", f.remote, "+"+f.base+":refs/heads/feature/preflight")
	before := f.snapshot()
	f.assertNewerAuthoritative("changed remote cached inspection")
	state := f.service.Refresh(f.ctx)
	if state.Pipeline.RunID != f.newerID || state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" ||
		state.Remote.Freshness != "live" || state.Remote.ObservedHead != f.base || state.NextAction == nil || state.NextAction.Code != "release_binding" {
		t.Fatalf("changed remote did not report the newer binding's ordinary refusal: %#v", state)
	}
	f.assertUnchanged(before, "changed remote refresh")
}

// TestPreflightSupersessionRefutesUnprovenOlderAttempts: every negative shape
// keeps the older run authoritative - the unproven attempt must never lose
// selection to a supersession it did not prove, and no ref or database row may
// change while the proof refuses.
func TestPreflightSupersessionRefutesUnprovenOlderAttempts(t *testing.T) {
	t.Parallel()

	refuted := func(name string, mutate func(f *preflightSupersessionFixture)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPreflightSupersessionFixture(t)
			mutate(f)
			before := f.snapshot()
			state := f.service.InspectCached(f.ctx)
			if state.Pipeline.RunID != f.olderID {
				t.Fatalf("negative %s: selected run=%s, want older %s; state=%s safety=%s", name, state.Pipeline.RunID, f.olderID, state.State, state.Safety)
			}
			f.assertUnchanged(before, "negative "+name)
		})
	}

	// The older attempt actually changed its head: not the never-started shape.
	refuted("older actually changed head", func(f *preflightSupersessionFixture) {
		mustRun(f.t, f.gate, "update-ref", "refs/heads/feature/preflight", f.submitted)
		if err := f.database.UpdateRunErrorStatusWithVerifiedHead(f.olderID, "explicit PR source repository/ref does not match the configured push target and run branch", types.RunFailed, f.pushed); err != nil {
			f.t.Fatal(err)
		}
		mustRun(f.t, f.gate, "update-ref", "refs/heads/feature/preflight", f.pushed)
	})
	// Any execution row refutes never-started.
	refuted("older has a step row", func(f *preflightSupersessionFixture) {
		if _, err := f.database.InsertStepResult(f.olderID, types.StepReview); err != nil {
			f.t.Fatal(err)
		}
	})
	refuted("older has an agent invocation", func(f *preflightSupersessionFixture) {
		inv := db.AgentInvocation{RunID: f.olderID, StepName: string(types.StepReview), Agent: "deck", Purpose: "review"}
		if _, err := f.database.InsertAgentInvocation(inv); err != nil {
			f.t.Fatal(err)
		}
	})
	// A non-preflight error is an uncertain launch route.
	refuted("older error is not the preflight route", func(f *preflightSupersessionFixture) {
		if err := f.database.UpdateRunError(f.olderID, "review failed for other reasons"); err != nil {
			f.t.Fatal(err)
		}
	})
	// A recorded worktree that still exists on disk refutes never-started.
	refuted("older worktree still exists", func(f *preflightSupersessionFixture) {
		live := filepath.Join(f.t.TempDir(), "live-worktree")
		if err := os.MkdirAll(live, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := f.database.SetRunWorktreeDir(f.olderID, live); err != nil {
			f.t.Fatal(err)
		}
	})
	refuted("older worktree is unreadable", func(f *preflightSupersessionFixture) {
		loop := filepath.Join(f.t.TempDir(), "worktree-loop")
		if err := os.Symlink(loop, loop); err != nil {
			f.t.Skipf("cannot create symlink: %v", err)
		}
		if _, err := os.Stat(loop); err == nil || os.IsNotExist(err) {
			f.t.Fatalf("expected an unreadable path, got %v", err)
		}
		if err := f.database.SetRunWorktreeDir(f.olderID, loop); err != nil {
			f.t.Fatal(err)
		}
	})
	// A push binding on the older run is execution evidence.
	refuted("older has a partial push binding", func(f *preflightSupersessionFixture) {
		f.execSQL(`UPDATE runs SET push_target_kind = ? WHERE id = ?`, "upstream", f.olderID)
	})
	refuted("older has a conflicting push binding", func(f *preflightSupersessionFixture) {
		if err := f.database.UpdateRunPushBinding(f.olderID, db.PushBinding{
			HeadSHA: f.base, TargetKind: "upstream", TargetFingerprint: "different-target", Ref: "refs/heads/feature/preflight",
		}); err != nil {
			f.t.Fatal(err)
		}
	})
	// The newer run must have submitted exactly the older attempt's head.
	refuted("newer submitted a different head", func(f *preflightSupersessionFixture) {
		f.execSQL(`UPDATE runs SET submitted_head_sha = ? WHERE id = ?`, f.base, f.newerID)
	})
	refuted("newer belongs to a different repository", func(f *preflightSupersessionFixture) {
		other, err := f.database.InsertRepo(filepath.Join(f.local, "other"), f.remote, "main")
		if err != nil {
			f.t.Fatal(err)
		}
		f.execSQL(`UPDATE runs SET repo_id = ? WHERE id = ?`, other.ID, f.newerID)
	})
	refuted("newer belongs to a different branch", func(f *preflightSupersessionFixture) {
		f.execSQL(`UPDATE runs SET branch = ?, push_ref = ? WHERE id = ?`, "feature/other", "refs/heads/feature/other", f.newerID)
	})
	// The newer run's terminal head must be verified.
	refuted("newer terminal head unverified", func(f *preflightSupersessionFixture) {
		if err := f.database.UpdateRunErrorStatus(f.newerID, "ci failed", types.RunFailed); err != nil {
			f.t.Fatal(err)
		}
		if err := f.database.UpdateRunPushBinding(f.newerID, db.PushBinding{
			HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/preflight",
		}); err != nil {
			f.t.Fatal(err)
		}
	})
	// The gate's exact branch head must equal the newer binding.
	refuted("gate head stale", func(f *preflightSupersessionFixture) {
		mustRun(f.t, f.local, "push", f.gate, "+"+f.base+":refs/heads/feature/preflight")
	})
	// A symbolic gate head is not exact non-symbolic evidence.
	refuted("gate head symbolic", func(f *preflightSupersessionFixture) {
		mustRun(f.t, f.gate, "symbolic-ref", "refs/heads/feature/preflight", "refs/heads/main")
	})
}

func (f *preflightSupersessionFixture) execSQL(query string, args ...any) {
	f.t.Helper()
	handle, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		f.t.Fatalf("open sqlite: %v", err)
	}
	defer handle.Close()
	if _, err := handle.Exec(query, args...); err != nil {
		f.t.Fatalf("update preflight fixture: %v", err)
	}
}

// TestPreflightSupersessionNewerActiveRunAlwaysWins: a newer ACTIVE run is
// never supersession evidence; it owns the branch absolutely, exactly as
// before this change.
func TestPreflightSupersessionNewerActiveRunAlwaysWins(t *testing.T) {
	t.Parallel()

	f := newPreflightSupersessionFixture(t)
	time.Sleep(1100 * time.Millisecond)
	active, err := f.database.InsertRun(f.repo.ID, "feature/preflight", f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	state := f.service.InspectCached(f.ctx)
	if state.Pipeline.RunID != active.ID || state.Safety != "blocked_pipeline_owned" {
		t.Fatalf("newer active run did not own the branch: run=%s state=%s safety=%s", state.Pipeline.RunID, state.State, state.Safety)
	}
	f.assertUnchanged(before, "active run")
}
