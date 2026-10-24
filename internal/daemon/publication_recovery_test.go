package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublicationRecoveryUsesPrimaryWithoutReviewRolesAndReusesPR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell agent fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, "gh"), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	log := filepath.Join(t.TempDir(), "forge.log")
	t.Setenv("FAKE_CLI_LOG", log)
	t.Setenv("FAKE_CLI_PUBLICATION_STATE", filepath.Join(t.TempDir(), "forge.json"))
	calls := filepath.Join(t.TempDir(), "primary.log")
	t.Setenv("PUBLICATION_PRIMARY_LOG", calls)
	t.Setenv("PUBLICATION_REFUSE", "1")
	primary := filepath.Join(bin, "claude")
	check(os.WriteFile(primary, []byte(`#!/bin/sh
prompt=$(cat)
digest=$(printf '%s\n' "$prompt" | sed -n 's/^- content_sha256: //p')
if [ -n "$digest" ]; then
  echo validate >> "$PUBLICATION_PRIMARY_LOG"
  mismatch=false
  issues='[]'
  if [ "$PUBLICATION_REFUSE" = 1 ]; then
    mismatch=true
    issues='["risk prose contradicts recorded repair"]'
  fi
  printf '{"type":"result","subtype":"success","structured_output":{"content_sha256":"%s","english":true,"format_compliant":true,"risk_disposition_mismatch":%s,"issues":%s}}\n' "$digest" "$mismatch" "$issues"
else
  echo draft >> "$PUBLICATION_PRIMARY_LOG"
  printf '%s\n' '{"type":"result","subtype":"success","structured_output":{"title":"fix: parser","body":"## What Changed\n\n- Restore records.","unsupported_rules":[]}}'
fi
`), 0o755))
	p := paths.WithRoot(t.TempDir())
	check(p.EnsureDirs())
	d, err := db.Open(p.DB())
	check(err)
	t.Cleanup(func() { _ = d.Close() })
	configYAML := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %q\n  pi: %q\n  codex: %q\nreview_agents:\n  reviewer: {agent: pi}\n  fixer: {agent: codex}\ntest:\n  evidence:\n    attach_media: false\n", primary, filepath.Join(bin, "missing-reviewer"), filepath.Join(bin, "missing-fixer"))
	check(os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644))
	repo, base := setupTestGitRepo(t, p, d, "publication-primary")
	gate := p.RepoDir(repo.ID)
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", gate)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	check(os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("restored records"), 0o644))
	gitCmd(t, repo.WorkingPath, "commit", "-am", "fix: restore records")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")
	t.Setenv("FAKE_CLI_PR_HEAD_SHA", head)
	run, err := d.InsertRun(repo.ID, "feature", head, base)
	check(err)
	check(d.UpdateRunReviewApprovedHeadSHA(run.ID, base))
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		row, err := d.InsertStepResult(run.ID, name)
		check(err)
		status := types.StepStatusCompleted
		if name == types.StepPR {
			status = types.StepStatusRunning
		} else if name == types.StepCI {
			status = types.StepStatusPending
		}
		check(d.UpdateStepStatus(row.ID, status))
		var findings *string
		switch name {
		case types.StepReview:
			text := `{"findings":[],"summary":"fix-only round","risk_level":"medium","risk_rationale":"The parser loses records.","resolved_by_fix":[{"id":"r1","severity":"error","description":"Restore records","action":"auto-fix"}]}`
			findings = &text
		case types.StepTest:
			text := fmt.Sprintf(`{"findings":[],"summary":"deterministic fixture, not live acceptance","tested":["fixture"],"testing_summary":"recorded fixture observation","artifacts":[],"scenarios":[{"name":"fixture","result":"pass","live":true,"evidence":"recorded fixture observation","reason":""}],"verdict":"go","tested_head_sha":%q}`, head)
			findings = &text
		}
		if findings != nil {
			check(d.SetStepFindings(row.ID, *findings))
		}
		if name != types.StepPR && name != types.StepCI {
			_, err := d.InsertStepRound(row.ID, 1, "initial", findings, nil, 1)
			check(err)
		}
	}
	check(d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(gate), Ref: "refs/heads/feature"}))
	run, err = d.GetRun(run.ID)
	check(err)
	mgr := NewRunManager(d, p, nil)
	cfg, err := mgr.loadRecoveredConfig(ctx, run, repo, repo.WorkingPath)
	check(err)
	ag, err := newConfiguredAgent(ctx, cfg, p.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(nil))
	check(err)
	defer ag.Close()
	sctx := &pipeline.StepContext{Ctx: ctx, Run: run, Repo: repo, DB: d, Config: cfg, Agent: ag, WorkDir: repo.WorkingPath, GateDir: gate, Log: func(string) {}, LogChunk: func(string) {}}
	_, refusal := (&steps.PRStep{}).Execute(sctx)
	if refusal == nil || !strings.Contains(refusal.Error(), "refusing non-compliant PR publication") {
		t.Fatalf("ordinary guard refusal=%v", refusal)
	}
	rows, err := d.GetStepsByRun(run.ID)
	check(err)
	for _, row := range rows {
		if row.StepName == types.StepPR {
			check(d.FailStep(row.ID, refusal.Error(), 1))
		}
	}
	check(d.UpdateRunErrorStatus(run.ID, "step pr failed: "+refusal.Error(), types.RunFailed))
	before, err := d.GetStepsByRun(run.ID)
	check(err)
	beforeRounds := make(map[string][]*db.StepRound)
	for _, row := range before {
		beforeRounds[row.ID], err = d.GetRoundsByStep(row.ID)
		check(err)
	}
	pin, err := d.GetPublicationIntent(run.ID, head)
	check(err)
	t.Setenv("PUBLICATION_REFUSE", "0")
	check(os.WriteFile(calls, nil, 0o644))
	params := &ipc.RecoverPublicationParams{RepoID: repo.ID, RunID: run.ID}
	result, err := mgr.HandleRecoverPublication(ctx, params)
	check(err)
	if result.PRURL != "https://github.com/test/repo/pull/99" {
		t.Fatalf("publication result=%+v", result)
	}
	invocations, err := os.ReadFile(calls)
	check(err)
	if string(invocations) != "draft\nvalidate\n" {
		t.Fatalf("primary invocations=%q", invocations)
	}
	check(os.WriteFile(calls, nil, 0o644))
	again, err := mgr.HandleRecoverPublication(ctx, params)
	check(err)
	if *again != *result {
		t.Fatalf("repeat=%+v, first=%+v", again, result)
	}
	invocations, err = os.ReadFile(calls)
	check(err)
	if len(invocations) != 0 {
		t.Fatalf("existing PR invoked primary: %s", invocations)
	}
	commands, err := os.ReadFile(log)
	check(err)
	if strings.Count(string(commands), "pr create") != 1 || strings.Contains(string(commands), "pr edit") {
		t.Fatalf("unexpected publication mutations: %s", commands)
	}
	after, err := d.GetStepsByRun(run.ID)
	check(err)
	afterPin, err := d.GetPublicationIntent(run.ID, head)
	check(err)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(pin, afterPin) {
		t.Fatal("publication changed step evidence or immutable pins")
	}
	terminal, err := d.GetRun(run.ID)
	check(err)
	if terminal.Status != types.RunFailed || terminal.HeadSHA != head || terminal.BaseSHA != base {
		t.Fatalf("recovery changed run identity: %+v", terminal)
	}
	for _, row := range after {
		rounds, err := d.GetRoundsByStep(row.ID)
		check(err)
		if !reflect.DeepEqual(beforeRounds[row.ID], rounds) {
			t.Fatalf("changed validation rounds for %s: %+v", row.StepName, rounds)
		}
	}
	if got := gitOutput(t, gate, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("recovery moved source branch: %s", got)
	}
}

func TestPublicationRecoveryRPCRefusesBeforeWorktreeOrRunMutation(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, head := setupTestGitRepo(t, p, d, "publication-refusal")
	run, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RecoverPublicationResult
	err = client.Call(ipc.MethodRecoverPublication, &ipc.RecoverPublicationParams{RepoID: repo.ID, RunID: run.ID}, &result)
	if err == nil || !strings.Contains(err.Error(), "terminal and failed") {
		t.Fatalf("RPC error=%v", err)
	}
	after, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != run.Status || after.HeadSHA != head || after.PRURL != nil {
		t.Fatalf("refusal changed run: %+v", after)
	}
	mgr := NewRunManager(d, p, nil)
	if _, err := mgr.HandleRecoverPublication(context.Background(), &ipc.RecoverPublicationParams{RepoID: "other", RunID: run.ID}); err == nil || !strings.Contains(err.Error(), "not found in this repository") {
		t.Fatalf("cross-repository recovery error=%v", err)
	}
}

func TestPublicationRecoveryRPCRejectsLegacyEvidenceWithoutRetrofitting(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, head := setupTestGitRepo(t, p, d, "legacy-publication-refusal")
	// Model the migration's historical default, not the new-run writer's
	// prospective eligibility marker. No historical consumer payload is invented.
	raw, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	const runID = "historical-publication-rpc-fixture"
	if _, err := raw.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES (?, ?, 'feature', ?, ?, 'failed', 1, 1)`, runID, repo.ID, head, head); err != nil {
		t.Fatal(err)
	}
	before, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeWorktrees, err := filepath.Glob(filepath.Join(p.Root(), "publication-recovery-*"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RecoverPublicationResult
	err = client.Call(ipc.MethodRecoverPublication, &ipc.RecoverPublicationParams{RepoID: repo.ID, RunID: runID}, &result)
	if err == nil || !strings.Contains(err.Error(), "historical run predates publication-intent pinning") {
		t.Fatalf("historical recovery error=%v", err)
	}
	t.Logf("recover_publication(%s): %v", runID, err)
	after, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || result.PRURL != "" {
		t.Fatalf("legacy refusal changed run or reported publication: before=%+v after=%+v result=%+v", before, after, result)
	}
	eligible, err := d.PublicationRecoveryEligible(runID)
	if err != nil || eligible {
		t.Fatalf("legacy eligibility changed: %v %v", eligible, err)
	}
	pin, err := d.GetPublicationIntent(runID, head)
	if err != nil || pin != nil {
		t.Fatalf("refusal retrofitted publication pins: %+v %v", pin, err)
	}
	afterWorktrees, err := filepath.Glob(filepath.Join(p.Root(), "publication-recovery-*"))
	if err != nil || !reflect.DeepEqual(beforeWorktrees, afterWorktrees) {
		t.Fatalf("legacy refusal created a recovery worktree: %v %v", afterWorktrees, err)
	}
}
