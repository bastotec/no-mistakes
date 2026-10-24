package branchsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These are producer provenance, not authority inferred from the lack of steps.
// This exact source revision binds submitted custody before checking the PR,
// and pins gates only after that check and before starting the executor.
const preflightProducer = "e6624fb"

type preflightFixture struct {
	*supersededFixture
	database *db.DB
	sql      *sql.DB
	dbPath   string
	svc      *Service
}

func newPreflightFixture(t *testing.T, anchor string) *preflightFixture {
	t.Helper()
	f := newSupersededFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, "", "init", "--bare", remote)
	mustRun(t, f.work, "push", remote, f.pushed+":refs/heads/"+f.branch)
	mustRun(t, f.work, "checkout", "-b", f.branch, f.olderSHA)
	// Reproduce each normal initial custody-anchor case, then the later
	// publication. No initial anchor is claimed never to have been written.
	ref := "refs/heads/" + f.branch
	mustRun(t, f.gate, "update-ref", "-d", ref)
	switch anchor {
	case "advance":
		mustRun(t, f.gate, "update-ref", ref, f.base)
	case "noop":
		mustRun(t, f.gate, "update-ref", ref, f.olderSHA)
	}
	mustRun(t, f.gate, "update-ref", ref, f.olderSHA)
	mustRun(t, f.gate, "update-ref", ref, f.pushed)
	// The generic supersession fixture's recovery ref is unrelated to these
	// actual database runs; remove it before taking snapshots.
	mustRun(t, f.gate, "update-ref", "-d", "refs/no-mistakes/recover/older-run")
	path := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	p := &preflightFixture{supersededFixture: f, database: database, sql: raw, dbPath: path}
	t.Cleanup(func() { p.database.Close(); raw.Close() })
	repo, err := database.InsertRepo(f.work, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	old, err := database.InsertRun(repo.ID, f.branch, f.olderSHA, f.olderSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunWorktreeDir(old.ID, filepath.Join(t.TempDir(), "removed-scratch")); err != nil {
		t.Fatal(err)
	}
	refusal := "explicit PR source repository/ref does not match the configured push target and run branch; " + f.branch + " publishes to https://github.com/upstream/widgets/pull/168 - if that is no longer the right target, run `no-mistakes axi run --retire-existing-pr` on the branch to return to ordinary discovery"
	if err := database.UpdateRunError(old.ID, refusal); err != nil {
		t.Fatal(err)
	}
	p.exec("UPDATE runs SET no_mistakes_build_sha=?, created_at=1 WHERE id=?", preflightProducer, old.ID)
	newer, err := database.InsertRun(repo.ID, f.branch, f.olderSHA, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(newer.ID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(newer.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(remote), Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, f.pushed); err != nil {
		t.Fatal(err)
	}
	p.exec("UPDATE runs SET created_at=2 WHERE id=?", newer.ID)
	f.older, _ = database.GetRun(old.ID)
	f.newer, _ = database.GetRun(newer.ID)
	p.svc = &Service{DB: database, Repo: repo, WorkDir: f.work, GateDir: f.gate}
	return p
}

func (f *preflightFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.sql.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// Snapshot every durable table, not only the fields the classifier reads.
func (f *preflightFixture) snapshot() string {
	f.t.Helper()
	tables, err := f.sql.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			f.t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()
	all := map[string][][]any{}
	for _, name := range names {
		rows, err := f.sql.Query(fmt.Sprintf("SELECT * FROM %q ORDER BY rowid", name))
		if err != nil {
			f.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				f.t.Fatal(err)
			}
			all[name] = append(all[name], values)
		}
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
		rows.Close()
	}
	data, err := json.Marshal(all)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func TestPreflightSupersessionSelectsVerifiedPublicationReadOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		anchor   string
		producer string
	}{
		{"create", preflightProducer},
		{"advance", preflightProducer},
		{"noop", preflightProducer},
		{"create", "e6624fb0"},
		{"create", "e6624fb072e9e029d727948640e441e9474ac418"},
	} {
		t.Run(tc.anchor+"/"+tc.producer, func(t *testing.T) {
			t.Parallel()
			f := newPreflightFixture(t, tc.anchor)
			f.exec("UPDATE runs SET no_mistakes_build_sha=? WHERE id=?", tc.producer, f.older.ID)
			before := f.snapshot()
			local := mustRun(t, f.work, "show-ref")
			gate := mustRun(t, f.gate, "show-ref")
			for reopen := 0; reopen < 2; reopen++ {
				state := f.svc.InspectCached(context.Background())
				if state.Pipeline.RunID != f.newer.ID {
					t.Fatalf("selected %s, want newer publication %s: %#v", state.Pipeline.RunID, f.newer.ID, state)
				}
				// The missing-head stack must use exactly the same supersession rule.
				ids, _, _, _ := f.svc.missingHeadKeepLocalRuns(f.ctx, &state, f.newer)
				for _, id := range ids {
					if id == f.older.ID {
						t.Fatal("obsolete preflight still obstructs missing-head stack")
					}
				}
				for _, keep := range []bool{false, true} {
					recovered := f.svc.Recover(f.ctx, keep)
					if recovered.Changed || recovered.Recovered || recovered.Pipeline.RunID != f.newer.ID {
						t.Fatalf("recovery altered obsolete custody: %#v", recovered)
					}
				}
				if got := f.snapshot(); got != before {
					t.Fatal("inspection/recovery rewrote durable history")
				}
				if mustRun(t, f.work, "show-ref") != local || mustRun(t, f.gate, "show-ref") != gate {
					t.Fatal("inspection/recovery changed refs")
				}
				if err := f.database.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := db.Open(f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				f.database = reopened
				f.svc.DB = reopened
			}
			refreshed := f.svc.Refresh(f.ctx)
			if refreshed.Pipeline.RunID != f.newer.ID || refreshed.State == StatePipelineOwned {
				t.Fatalf("refresh still obstructed by preflight: %#v", refreshed)
			}
			if got := f.snapshot(); got != before {
				t.Fatal("refresh rewrote durable history")
			}
			if mustRun(t, f.gate, "show-ref") != gate {
				t.Fatal("refresh changed managed gate refs")
			}
			// Refresh can fetch its private observation ref, but cannot move the caller.
			if mustRun(t, f.work, "rev-parse", "HEAD") != f.olderSHA {
				t.Fatal("refresh moved caller")
			}
		})
	}
}

func TestPreflightSupersessionRejectsUncertainOrUniqueCustody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*preflightFixture)
	}{
		{"changed old head", func(f *preflightFixture) { f.exec("UPDATE runs SET head_sha=? WHERE id=?", f.base, f.older.ID) }},
		{"empty submitted", func(f *preflightFixture) {
			f.exec("UPDATE runs SET head_sha='',submitted_head_sha='' WHERE id=?", f.older.ID)
		}},
		{"unknown producer", func(f *preflightFixture) {
			f.exec("UPDATE runs SET no_mistakes_build_sha='unknown' WHERE id=?", f.older.ID)
		}},
		{"shorter producer prefix", func(f *preflightFixture) {
			f.exec("UPDATE runs SET no_mistakes_build_sha='e6624f' WHERE id=?", f.older.ID)
		}},
		{"different producer with shared prefix", func(f *preflightFixture) {
			f.exec("UPDATE runs SET no_mistakes_build_sha='e6624fb1' WHERE id=?", f.older.ID)
		}},
		{"missing producer", func(f *preflightFixture) { f.exec("UPDATE runs SET no_mistakes_build_sha=NULL WHERE id=?", f.older.ID) }},
		{"missing positive failure route", func(f *preflightFixture) { f.exec("UPDATE runs SET error='setup failed' WHERE id=?", f.older.ID) }},
		{"missing worktree placement", func(f *preflightFixture) { f.exec("UPDATE runs SET worktree_dir=NULL WHERE id=?", f.older.ID) }},
		{"gate pin before executor", func(f *preflightFixture) {
			if err := f.database.SetRunGates(f.older.ID, "[]"); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"step allocated", func(f *preflightFixture) {
			if _, err := f.database.InsertStepResult(f.older.ID, types.StepRebase); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"agent invocation", func(f *preflightFixture) {
			if _, err := f.database.InsertAgentInvocation(db.AgentInvocation{RunID: f.older.ID, StepName: "review", Agent: "deck", Purpose: "review"}); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"session", func(f *preflightFixture) {
			if err := f.database.UpsertRunAgentSession(f.older.ID, "fixer", "deck", "session"); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"launch nonce", func(f *preflightFixture) { f.exec("UPDATE runs SET launch_nonce='opaque' WHERE id=?", f.older.ID) }},
		{"partial launch proof", func(f *preflightFixture) {
			f.exec("UPDATE runs SET launch_validation_generation='opaque' WHERE id=?", f.older.ID)
		}},
		{"push active", func(f *preflightFixture) { f.exec("UPDATE runs SET push_active=1 WHERE id=?", f.older.ID) }},
		{"partial push ref", func(f *preflightFixture) {
			f.exec("UPDATE runs SET push_ref=? WHERE id=?", "refs/heads/"+f.branch, f.older.ID)
		}},
		{"partial push time", func(f *preflightFixture) { f.exec("UPDATE runs SET last_pushed_at=1 WHERE id=?", f.older.ID) }},
		{"old publication", func(f *preflightFixture) {
			f.exec("UPDATE runs SET last_pushed_sha=? WHERE id=?", f.olderSHA, f.older.ID)
		}},
		{"custody stamp", func(f *preflightFixture) { f.exec("UPDATE runs SET custody_returned_at=1 WHERE id=?", f.older.ID) }},
		{"newer different submitted", func(f *preflightFixture) {
			f.exec("UPDATE runs SET submitted_head_sha=? WHERE id=?", f.base, f.newer.ID)
		}},
		{"newer unverified", func(f *preflightFixture) {
			f.exec("UPDATE runs SET terminal_head_verified_at=NULL WHERE id=?", f.newer.ID)
		}},
		{"newer live push", func(f *preflightFixture) { f.exec("UPDATE runs SET push_active=1 WHERE id=?", f.newer.ID) }},
		{"newer partial binding", func(f *preflightFixture) { f.exec("UPDATE runs SET push_generation=NULL WHERE id=?", f.newer.ID) }},
		{"newer changed target", func(f *preflightFixture) {
			f.exec("UPDATE runs SET push_target_fingerprint='other' WHERE id=?", f.newer.ID)
		}},
		{"newer changed branch", func(f *preflightFixture) { f.exec("UPDATE runs SET branch='other' WHERE id=?", f.newer.ID) }},
		{"newer changed repository", func(f *preflightFixture) {
			repo, err := f.database.InsertRepo(filepath.Join(f.work, "other"), "https://example.invalid/other.git", "main")
			if err != nil {
				f.t.Fatal(err)
			}
			f.exec("UPDATE runs SET repo_id=? WHERE id=?", repo.ID, f.newer.ID)
		}},
		{"not strictly newer", func(f *preflightFixture) { f.exec("UPDATE runs SET created_at=0 WHERE id=?", f.newer.ID) }},
		{"stale gate", func(f *preflightFixture) { mustRun(f.t, f.gate, "update-ref", "refs/heads/"+f.branch, f.olderSHA) }},
		{"symbolic gate", func(f *preflightFixture) {
			mustRun(f.t, f.gate, "update-ref", "refs/heads/alias", f.pushed)
			mustRun(f.t, f.gate, "symbolic-ref", "refs/heads/"+f.branch, "refs/heads/alias")
		}},
		{"unique recovery ref", func(f *preflightFixture) { mustRun(f.t, f.gate, "update-ref", recoverAnchorRef(f.older.ID), f.pushed) }},
		{"unique local recovery ref", func(f *preflightFixture) {
			mustRun(f.t, f.work, "update-ref", recoverLocalAnchorRef(f.older.ID), f.base)
		}},
		{"unique captured handoff", func(f *preflightFixture) {
			source, err := f.database.InsertRun(f.svc.Repo.ID, "other", f.base, f.base)
			if err != nil {
				f.t.Fatal(err)
			}
			if err := f.database.SetRunSurvivedWork(source.ID, &types.SurvivedWork{State: types.SurvivedWorkCaptured, Ref: "refs/no-mistakes/survived/source", BaseSHA: f.base}); err != nil {
				f.t.Fatal(err)
			}
			if err := f.database.CopyRunSurvivedWork(source.ID, f.older.ID, *f.older.WorktreeDir); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"unique dirty scratch", func(f *preflightFixture) {
			mustRun(f.t, f.gate, "worktree", "add", "--detach", *f.older.WorktreeDir, f.olderSHA)
			mustWrite(f.t, filepath.Join(*f.older.WorktreeDir, "unlanded.txt"), "unique\n")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPreflightFixture(t, "create")
			tc.change(f)
			before := f.snapshot()
			local := mustRun(t, f.work, "show-ref")
			gate := mustRun(t, f.gate, "show-ref")
			for reopen := 0; reopen < 2; reopen++ {
				old, err := f.database.GetRun(f.older.ID)
				if err != nil {
					t.Fatal(err)
				}
				newer, err := f.database.GetRun(f.newer.ID)
				if err != nil {
					t.Fatal(err)
				}
				if f.svc.supersededPreflightRun(f.ctx, old, newer, f.branch) {
					t.Fatal("uncertain preflight accepted")
				}
				state := f.svc.InspectCached(f.ctx)
				// Classification can report ordinary custody returned or push-in-progress;
				// it must not silently select the newer publication via the new exception.
				if unpublishedPipelineHead(old) && state.Pipeline.RunID != old.ID {
					t.Fatalf("uncertain older run skipped: %#v", state)
				}
				ids, _, _, _ := f.svc.missingHeadKeepLocalRuns(f.ctx, &state, old)
				if unpublishedPipelineHead(old) {
					found := false
					for _, id := range ids {
						found = found || id == old.ID
					}
					if !found {
						t.Fatal("uncertain preflight omitted from missing-head custody stack")
					}
				}
				if mustRun(t, f.work, "show-ref") != local {
					t.Fatal("cached inspection changed local refs")
				}
				refreshed := f.svc.Refresh(f.ctx)
				if refreshed.Changed || unpublishedPipelineHead(old) && refreshed.Pipeline.RunID != old.ID {
					t.Fatalf("refresh bypassed uncertain custody: %#v", refreshed)
				}
				if f.snapshot() != before || mustRun(t, f.work, "rev-parse", "HEAD") != f.olderSHA || mustRun(t, f.gate, "show-ref") != gate {
					t.Fatal("read-only refusal changed database or custody refs")
				}
				local = mustRun(t, f.work, "show-ref")
				if err := f.database.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := db.Open(f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				f.database = reopened
				f.svc.DB = reopened
			}
		})
	}
}

func TestPreflightSupersessionPreservesActiveRemoteDirtyAndDivergenceGuards(t *testing.T) {
	t.Parallel()
	t.Run("newer active ownership", func(t *testing.T) {
		t.Parallel()
		f := newPreflightFixture(t, "create")
		active, err := f.database.InsertRun(f.svc.Repo.ID, f.branch, f.olderSHA, f.base)
		if err != nil {
			t.Fatal(err)
		}
		f.exec("UPDATE runs SET created_at=3 WHERE id=?", active.ID)
		before := f.snapshot()
		gate := mustRun(t, f.gate, "show-ref")
		state := f.svc.InspectCached(f.ctx)
		if state.Pipeline.RunID != active.ID {
			t.Fatalf("active owner lost: %#v", state)
		}
		recovered := f.svc.Recover(f.ctx, true)
		if recovered.Changed || recovered.Safety != "blocked_recover_run_active" {
			t.Fatalf("active recovery: %#v", recovered)
		}
		if before != f.snapshot() || gate != mustRun(t, f.gate, "show-ref") {
			t.Fatal("active ownership observation mutated custody")
		}
	})
	t.Run("remote moved", func(t *testing.T) {
		t.Parallel()
		f := newPreflightFixture(t, "create")
		mustRun(t, f.work, "push", f.svc.Repo.PushURL(), "+"+f.base+":refs/heads/"+f.branch)
		before := f.snapshot()
		gate := mustRun(t, f.gate, "show-ref")
		state := f.svc.Refresh(f.ctx)
		if state.Pipeline.RunID != f.newer.ID || !strings.HasPrefix(state.Safety, "blocked_remote_") {
			t.Fatalf("remote change hidden: %#v", state)
		}
		if before != f.snapshot() || gate != mustRun(t, f.gate, "show-ref") {
			t.Fatal("remote refusal mutated custody")
		}
	})
	t.Run("dirty caller", func(t *testing.T) {
		t.Parallel()
		f := newPreflightFixture(t, "create")
		mustWrite(t, filepath.Join(f.work, "uncommitted.txt"), "local work\n")
		before := f.snapshot()
		state := f.svc.Refresh(f.ctx)
		if state.Pipeline.RunID != f.newer.ID || state.State != StateDirty {
			t.Fatalf("dirty caller bypassed: %#v", state)
		}
		if before != f.snapshot() {
			t.Fatal("dirty caller observation changed durable history")
		}
	})
	t.Run("genuine post selection divergence", func(t *testing.T) {
		t.Parallel()
		f := newPreflightFixture(t, "create")
		mustWrite(t, filepath.Join(f.work, "feature.txt"), "operator followup conflicts\n")
		mustRun(t, f.work, "commit", "-am", "caller followup")
		head := mustRun(t, f.work, "rev-parse", "HEAD")
		before := f.snapshot()
		gate := mustRun(t, f.gate, "show-ref")
		state := f.svc.Refresh(f.ctx)
		if state.Pipeline.RunID != f.newer.ID || state.Safety != "blocked_diverged" {
			t.Fatalf("genuine divergence hidden: %#v", state)
		}
		recovered := f.svc.Recover(f.ctx, false)
		if recovered.Changed || before != f.snapshot() || gate != mustRun(t, f.gate, "show-ref") || head != mustRun(t, f.work, "rev-parse", "HEAD") {
			t.Fatal("selection exception reconciled distinct caller work")
		}
	})
}
