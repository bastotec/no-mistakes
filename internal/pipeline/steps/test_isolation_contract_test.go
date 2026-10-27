package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestStepIsolationRefusesUnsupportedAgentBeforeAnyCommand(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	marker := filepath.Join(dir, "must-not-run")
	ag := &mockAgent{name: "unsupported", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent executed despite isolation refusal")
		return nil, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: "echo forbidden > must-not-run"})
	if err := sctx.DB.EnableTestIsolation(sctx.Run.ID); err != nil {
		t.Fatal(err)
	}
	_, err := (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "test-isolation-adapter") {
		t.Fatalf("unsupported adapter did not refuse: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran before refusal")
	}
}
func TestIsolatedTestWorkerFailurePreservesCauseDiagnosticsAndCustody(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("Linux unprivileged worker launcher")
	}
	for _, mode := range []string{"startup", "input", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\n"
			if mode != "input" {
				body += "IFS= read -r request\n"
			}
			if mode == "shutdown" {
				body += "printf '%s\\n' '{\"Kind\":\"log\",\"Text\":\"worker ready\"}'\nexec /bin/sleep 30\n"
			} else {
				body += "echo 'namespace prerequisite refused' >&2\nexit 23\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "bwrap"), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"deck", "initdb", "postgres", "psql"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			key := filepath.Join(t.TempDir(), "model-key")
			if err := os.WriteFile(key, []byte("fixture-only"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("NM_TEST_POSTGRES_ROOT", root)
			t.Setenv("NM_TEST_MODEL_URL", "http://127.0.0.1:1/v1")
			t.Setenv("NM_TEST_MODEL_KEY_FILE", key)
			t.Setenv("NM_TEST_MODEL", "fixture-model")
			source, base, head := setupGitRepo(t)
			work := filepath.Join(t.TempDir(), "work")
			gitCmd(t, source, "worktree", "add", "--detach", work, head)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, work, base, head, config.Commands{})
			step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"test-1","severity":"error","action":"auto-fix","description":"repair"}]}`
			round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if decision, err := sctx.DB.ReserveStepRepairWithSelection(step.ID, round.ID, 1, nil); err != nil || !decision.Granted {
				t.Fatalf("reserve repair: %+v %v", decision, err)
			}
			if err := sctx.DB.ClaimStepRepair(step.ID, round.ID); err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID, sctx.Fixing = step.ID, true
			sctx.IsolatedRepairRound = func() string { return round.ID }
			deadline, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			ctx, cancel := context.WithCancelCause(deadline)
			defer cancel(nil)
			sctx.Ctx = ctx
			if mode == "shutdown" {
				sctx.Log = func(text string) {
					if text == "worker ready" {
						cancel(pipeline.ErrDaemonShutdown)
					}
				}
			} else if mode == "input" {
				sctx.UserIntent = strings.Repeat("x", 1<<20)
			}
			outcome, err := executeIsolatedTest(sctx)
			if outcome != nil || !errors.Is(err, pipeline.ErrIsolatedTestHandoff) {
				t.Fatalf("incomplete handoff lost: %+v %v", outcome, err)
			}
			if mode == "shutdown" {
				if !errors.Is(err, pipeline.ErrDaemonShutdown) {
					t.Fatalf("shutdown cause lost: %v", err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.Contains(err.Error(), "namespace prerequisite refused") {
					t.Fatalf("worker diagnostic lost: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(sctx.EvidenceDir), "."+sctx.Run.ID+"-test-isolation", "git")); err != nil {
				t.Fatalf("interrupted workspace lost: %v", err)
			}
			called := false
			_, err = sctx.RunIsolatedTestRepair(func(func(int) error) (*pipeline.StepOutcome, error) {
				called = true
				return nil, nil
			})
			if called || err == nil || !strings.Contains(err.Error(), "refusing to replay") {
				t.Fatalf("started repair custody lost: called=%v err=%v", called, err)
			}
		})
	}
}

func TestIsolatedTestWorkerConfiguredPreparationUsesPrivateGit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	source, base, head := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte(".deps/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", ".gitignore")
	gitCmd(t, source, "commit", "-m", "ignore prepared dependencies")
	head = gitCmd(t, source, "rev-parse", "HEAD")
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, source, "worktree", "add", "--detach", work, head)
	evidenceCalls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		evidenceCalls++
		if _, err := os.Stat(filepath.Join(opts.CWD, ".deps", "count")); err != nil {
			t.Fatalf("prepared dependencies unavailable: %v", err)
		}
		if got := gitCmd(t, work, "show", ":pending_test.go"); got != "staged bytes" {
			t.Fatalf("preparation lost staged bytes: %q", got)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"prepared","tested":["dependency check"],"testing_summary":"dependencies available","artifacts":[],"scenarios":[{"name":"configured command","result":"pass","live":true,"evidence":"dependency check","reason":""}],"verdict":"go","risk_level":"low","risk_rationale":"driven live at the tested head with nothing untested"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, work, base, head, config.Commands{
		Prepare: preparationCommand(), Test: dependencyExistsCommand() + " && echo baseline > .deps/baseline",
	})
	if err := os.WriteFile(filepath.Join(work, "pending_test.go"), []byte("staged bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "pending_test.go")
	workspace, err := testisolation.PrepareGit(sctx.Ctx, work, t.TempDir(), base, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+workspace.Dir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sctx.GateDir, sctx.IsolatedTestWorker, sctx.Shared = workspace.Dir, true, &pipeline.RunShared{}
	if _, err := snapshotPreparationState(sctx.Ctx, work, sctx.GateDir); err == nil {
		t.Fatal("ordinary preparation accepted non-bare Git storage")
	}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || outcome == nil || outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("configured isolated Test: %+v %v", outcome, err)
	}
	if evidenceCalls != 1 {
		t.Fatalf("evidence calls = %d", evidenceCalls)
	}
	if _, err := os.Stat(filepath.Join(work, ".deps", "baseline")); err != nil {
		t.Fatalf("baseline did not run: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "base.txt")); err != nil || string(got) != "base content" {
		t.Fatalf("preparation mutation survived: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(work, "prepare.tmp")); !os.IsNotExist(err) {
		t.Fatalf("preparation artifact survived: %v", err)
	}
	if got := gitCmd(t, source, "rev-parse", "HEAD"); got != head {
		t.Fatal("worker changed shared head")
	}
}

func TestIsolatedTestNoOpFixPreservesCheckoutNormalization(t *testing.T) {
	for _, mode := range []string{"autocrlf", "attributes"} {
		t.Run(mode, func(t *testing.T) {
			testIsolatedTestNoOpFixPreservesCheckoutNormalization(t, mode)
		})
	}
}

func testIsolatedTestNoOpFixPreservesCheckoutNormalization(t *testing.T, mode string) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	globalConfig := "[core]\nautocrlf = true\n"
	if mode == "attributes" {
		external := filepath.Join(root, "host-attributes")
		if err := os.WriteFile(external, []byte("*.txt text eol=crlf filter=host\nprecedence.txt -text\n"), 0600); err != nil {
			t.Fatal(err)
		}
		globalConfig = "[core]\nautocrlf = false\nattributesFile = " + external + "\n"
	}
	if err := os.WriteFile(global, []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	source, base, _ := setupGitRepo(t)
	files := map[string][]byte{
		"normalized.txt": []byte("first line\nsecond line\n"),
		"binary.dat":     []byte("binary\x00\nunchanged\n"),
	}
	if mode == "attributes" {
		files["info.dat"] = []byte("info normalized\n")
		files["precedence.txt"] = []byte("info takes precedence\n")
		files["repo.txt"] = []byte("repository takes precedence\n")
		files[".gitattributes"] = []byte("repo.txt text eol=lf\n")
		info := filepath.Join(source, ".git", "info", "attributes")
		if err := os.WriteFile(info, []byte("info.dat text eol=crlf\nprecedence.txt text eol=crlf\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	add := []string{"add"}
	for name := range files {
		add = append(add, name)
	}
	gitCmd(t, source, add...)
	gitCmd(t, source, "commit", "-m", "normalization fixture")
	head := gitCmd(t, source, "rev-parse", "HEAD")
	work := filepath.Join(root, "work")
	gitCmd(t, source, "worktree", "add", "--detach", work, head)
	checkout := map[string][]byte{}
	for name := range files {
		data, err := os.ReadFile(filepath.Join(work, name))
		if err != nil {
			t.Fatal(err)
		}
		checkout[name] = data
	}
	if !bytes.Equal(checkout["normalized.txt"], []byte("first line\r\nsecond line\r\n")) || !bytes.Equal(checkout["binary.dat"], files["binary.dat"]) {
		t.Fatalf("unexpected checkout bytes: %q", checkout)
	}
	if mode == "attributes" {
		for _, name := range []string{"info.dat", "precedence.txt"} {
			if !bytes.Equal(checkout[name], bytes.ReplaceAll(files[name], []byte("\n"), []byte("\r\n"))) {
				t.Fatalf("info attributes did not normalize %s: %q", name, checkout[name])
			}
		}
		if !bytes.Equal(checkout["repo.txt"], files["repo.txt"]) {
			t.Fatalf("external attributes overrode repository attributes: %q", checkout["repo.txt"])
		}
	}
	marker := filepath.Join(root, "host-filter-executed")
	if mode == "attributes" {
		gitCmd(t, source, "config", "filter.host.clean", "printf called > '"+marker+"'; cat")
		gitCmd(t, source, "config", "filter.host.smudge", "printf called > '"+marker+"'; cat")
		gitCmd(t, source, "config", "filter.host.required", "true")
	}
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"summary":"no changes needed"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, work, base, head, config.Commands{})
	workspace, err := testisolation.PrepareGit(sctx.Ctx, work, t.TempDir(), base, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+workspace.Dir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "attributes" {
		for _, path := range []string{filepath.Join(root, "host-attributes"), filepath.Join(source, ".git", "info", "attributes")} {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	sctx.GateDir, sctx.IsolatedTestWorker, sctx.Fixing = workspace.Dir, true, true
	summary, err := executeFixMode(sctx, types.StepTest, fixExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || summary != noChangesAppliedSummary {
		t.Fatalf("no-op fixer produced changes: calls=%d summary=%q", calls, summary)
	}
	if got := gitCmd(t, work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("no-op fixer advanced HEAD: %s", got)
	}
	for name, want := range files {
		blob, err := git.RunRawWithInput(sctx.Ctx, work, "", "show", "HEAD:"+name)
		if err != nil || !bytes.Equal(blob, want) {
			t.Fatalf("committed blob %s changed: %q %v", name, blob, err)
		}
		data, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || !bytes.Equal(data, checkout[name]) {
			t.Fatalf("checkout %s rewritten: %q %v", name, data, err)
		}
	}
	if status := gitCmd(t, work, "status", "--porcelain"); status != "" {
		t.Fatalf("no-op fixer left normalization noise: %s", status)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host attribute filter executed: %v", err)
	}
}

func TestIsolatedTestWorkerProtectedPathRetryRetainsStagedOnlyBlobs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	source, base, head := setupGitRepo(t)
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, source, "worktree", "add", "--detach", work, head)
	pointer, err := os.ReadFile(filepath.Join(work, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			for name, content := range map[string]string{"protected.lock": "protected edit\n", "staged_test.go": "retained blob\n"} {
				if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, work, "add", "protected.lock", "staged_test.go")
			if err := os.Remove(filepath.Join(work, "staged_test.go")); err != nil {
				t.Fatal(err)
			}
		} else if got := gitCmd(t, work, "show", ":staged_test.go"); got != "retained blob" {
			t.Fatalf("retry lost staged-only content: %q", got)
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair test failure"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, work, base, head, config.Commands{})
	sctx.IsolatedTestWorker, sctx.Fixing = true, true
	sctx.Config.ProtectedPaths = []string{"protected.lock"}
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"repair failure","action":"auto-fix"}]}`
	for range 2 {
		workspace, err := testisolation.PrepareGit(sctx.Ctx, work, t.TempDir(), base, "main")
		if err != nil {
			t.Fatalf("prepare retry: %v", err)
		}
		sctx.GateDir = workspace.Dir
		func() {
			if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+workspace.Dir+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(filepath.Join(work, ".git"), pointer, 0600); err != nil {
					t.Fatal(err)
				}
			}()
			_, err := (&TestStep{}).Execute(sctx)
			var refusal *pipeline.ProtectedPathError
			if !errors.As(err, &refusal) || refusal.Path != "protected.lock" {
				t.Fatalf("expected protected-path refusal: %v", err)
			}
		}()
		index, err := testisolation.ExportGitIndex(sctx.Ctx, workspace.Dir, head)
		if err != nil {
			t.Fatal(err)
		}
		if err := testisolation.ImportGitIndex(sctx.Ctx, work, index); err != nil {
			t.Fatal(err)
		}
		if got := gitCmd(t, work, "show", ":staged_test.go"); got != "retained blob" {
			t.Fatalf("handoff lost staged-only content: %q", got)
		}
		if got := gitCmd(t, work, "rev-parse", "HEAD"); got != head {
			t.Fatal("refused repair advanced host head")
		}
	}
	if calls != 2 {
		t.Fatalf("repair calls = %d", calls)
	}
}

func TestTestStepOrdinaryRunKeepsAmbientEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX environment assertion")
	}
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "ordinary-sentinel")
	t.Setenv("NM_TEST_MODEL_URL", "")
	t.Setenv("NM_TEST_POSTGRES_ROOT", "")
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "ordinary", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ordinary","tested":["baseline"],"testing_summary":"baseline","artifacts":[],"scenarios":[{"name":"ordinary","result":"pass","live":true,"evidence":"fixture","reason":""}],"verdict":"go","risk_level":"low","risk_rationale":"driven live at the tested head with nothing untested"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: `test "$UNRECOGNIZED_CREDENTIAL" = ordinary-sentinel`})
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || outcome == nil || outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("ordinary behavior changed: %+v %v", outcome, err)
	}
}

func TestIsolatedTestErrorProtocolPreservesExecutorClassifications(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("repair: %w", pipeline.ErrTestAgentTimeout), &pipeline.ProtectedPathError{Path: "secrets.yaml", Rule: "secrets.*"}} {
		var wire bytes.Buffer
		if err := json.NewEncoder(&wire).Encode(isolatedStepEvent{Kind: "error", Error: pipeline.EncodeIsolatedTestError(failure)}); err != nil {
			t.Fatal(err)
		}
		var received isolatedStepEvent
		if err := json.NewDecoder(&wire).Decode(&received); err != nil {
			t.Fatal(err)
		}
		restored := received.Error.Restore()
		if errors.Is(failure, pipeline.ErrTestAgentTimeout) {
			if !errors.Is(restored, pipeline.ErrTestAgentTimeout) {
				t.Fatalf("timeout flattened: %v", restored)
			}
		} else {
			var refusal *pipeline.ProtectedPathError
			if !errors.As(restored, &refusal) || refusal.Path != "secrets.yaml" || refusal.Rule != "secrets.*" || pipeline.ProtectedPathOutcome(restored) == nil {
				t.Fatalf("refusal flattened: %v", restored)
			}
		}
	}
}

func TestIsolatedTestDecisionProtocolOmitsProvenanceWithoutMutatingHistory(t *testing.T) {
	findings := `{"findings":[{"id":"declined","severity":"warning","description":"standing decision","action":"ask-user"}]}`
	selected, source := "[]", db.RoundSelectionSourceUserDeclined
	original := []*db.BranchDecisionRound{{RunID: "prior", StepName: types.StepReview, Round: &db.StepRound{Round: 2, Trigger: "initial", FindingsJSON: &findings,
		SelectedFindingIDs: &selected, SelectionSource: &source, GlobalConfigYAML: []byte("operator-secret"), RepoConfigYAML: []byte("repository-secret")}}}
	input := isolatedTestInput{PriorBranchDecisions: isolatedBranchDecisions(original)}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var received isolatedTestInput
	if err := json.Unmarshal(payload, &received); err != nil {
		t.Fatal(err)
	}
	if len(received.PriorBranchDecisions) != 1 {
		t.Fatal("decision omitted")
	}
	round := received.PriorBranchDecisions[0].Round
	if len(round.GlobalConfigYAML) != 0 || len(round.RepoConfigYAML) != 0 {
		t.Fatal("secret provenance crossed worker input")
	}
	before := branchDecisionsPromptSection(&pipeline.StepContext{PriorBranchDecisions: original})
	after := branchDecisionsPromptSection(&pipeline.StepContext{PriorBranchDecisions: received.PriorBranchDecisions})
	if before == "" || after != before {
		t.Fatalf("decision meaning changed: before=%q after=%q", before, after)
	}
	if string(original[0].Round.GlobalConfigYAML) != "operator-secret" || string(original[0].Round.RepoConfigYAML) != "repository-secret" {
		t.Fatal("live history mutated")
	}
}

func TestIsolatedTestDeckResolutionUsesSelectedOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-deck")
	if err := os.WriteFile(custom, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, selected := range []string{custom, "custom-deck"} {
		resolved, err := isolatedDeckPath(&config.Config{AgentPathOverride: map[string]string{"deck": selected}})
		if err != nil || resolved != custom {
			t.Fatalf("selected %q: got %q %v", selected, resolved, err)
		}
	}
	if _, err := isolatedDeckPath(&config.Config{}); err == nil {
		t.Fatal("absent default Deck accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "deck"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedDeckPath(&config.Config{AgentPathOverride: map[string]string{"deck": "missing-custom"}}); err == nil {
		t.Fatal("invalid explicit selection fell back to Deck")
	}
}

func TestIsolatedTestInvocationAndActivityProtocol(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.IsolatedRound = func() int { return 4 }
	activities := 0
	sctx.IsolatedActivity = func() { activities++ }
	var wire bytes.Buffer
	encoder := json.NewEncoder(&wire)
	calls := 0
	ag := &mockAgent{name: "deck", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		opts.OnLifecycle(agent.LifecycleEvent{Agent: "deck", Phase: agent.LifecyclePhaseActivity, PID: 99, Message: "private-tool-arguments"})
		if calls == 2 {
			return nil, errors.New("deck exited with private-tool-arguments")
		}
		now := time.Now()
		opts.OnAttempt(agent.Attempt{Agent: "deck", Err: errors.New("deck exited"), StartedAt: now.Add(-2 * time.Second), CompletedAt: now.Add(-time.Second)})
		result := &agent.Result{Text: "private-agent-output", Output: json.RawMessage(`{"findings":[]}`), SessionID: "private-session", Model: "fixture-model",
			UsageReported: true, Usage: agent.TokenUsage{InputTokens: 123, OutputTokens: 45, CacheReadTokens: 12}, Metrics: &agent.InvocationMetrics{ToolCalls: 2}}
		opts.OnAttempt(agent.Attempt{Agent: "deck", Result: result, StartedAt: now.Add(-time.Second), CompletedAt: now})
		return result, nil
	}}
	snapshotPath := filepath.Join(t.TempDir(), "worker.db")
	if err := sctx.DB.ExportTestSnapshot(snapshotPath, sctx.Run.ID); err != nil {
		t.Fatal(err)
	}
	workerDB, err := db.Open(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	defer workerDB.Close()
	wrapped := pipeline.InstrumentIsolatedTestAgent(ag, workerDB, sctx.Run.ID, 1,
		func(inv db.AgentInvocation) {
			if err := encoder.Encode(isolatedStepEvent{Kind: "invocation", Invocation: &inv}); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			if err := encoder.Encode(isolatedStepEvent{Kind: "activity"}); err != nil {
				t.Fatal(err)
			}
		})
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "test-evidence", Prompt: "private-prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "test-fix"}); err == nil {
		t.Fatal("fixture failure missing")
	}
	for _, secret := range []string{"private-tool-arguments", "private-agent-output", "private-session", "private-prompt"} {
		if bytes.Contains(wire.Bytes(), []byte(secret)) {
			t.Fatalf("private agent content crossed observation protocol: %s", secret)
		}
	}
	before, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil || len(before) != 0 {
		t.Fatalf("worker wrote live database: %+v %v", before, err)
	}
	decoder := json.NewDecoder(&wire)
	for {
		var event isolatedStepEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := applyIsolatedStepObservation(sctx, types.StepTest, event); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("invocations lost: %+v %v", rows, err)
	}
	known, unknown := 0, 0
	for _, row := range rows {
		if row.Round != 4 || row.StepName != string(types.StepTest) || row.RunID != sctx.Run.ID {
			t.Fatalf("worker controlled invocation scope: %+v", row)
		}
		if row.InputTokens == nil {
			unknown++
			continue
		}
		known++
		if *row.InputTokens != 123 || row.OutputTokens == nil || *row.OutputTokens != 45 || row.ToolCalls == nil || *row.ToolCalls != 2 || row.DurationMS != 1000 {
			t.Fatalf("invocation metrics changed: %+v", row)
		}
	}
	if activities != 2 || known != 1 || unknown != 2 {
		t.Fatalf("observations lost: activities=%d known=%d unknown=%d", activities, known, unknown)
	}
}

func TestIsolatedWorkerHandoffPreservesHeadIndexAndReviewProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("protocol fixture requires a POSIX shell")
	}
	for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepLint} {
		t.Run(string(stepName), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", head)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, dir, base, head, config.Commands{})
			sctx.Fixing, sctx.ReviewStartingHeadSHA = true, head
			workerDir := filepath.Join(t.TempDir(), "worker")
			gitCmd(t, dir, "clone", "--no-hardlinks", dir, workerDir)
			gitCmd(t, workerDir, "config", "user.name", "test")
			gitCmd(t, workerDir, "config", "user.email", "test@test.com")
			if err := os.WriteFile(filepath.Join(workerDir, "fixed.txt"), []byte("fixed"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, workerDir, "add", "fixed.txt")
			gitCmd(t, workerDir, "commit", "-m", "worker fix")
			fixedHead := gitCmd(t, workerDir, "rev-parse", "HEAD")
			pack, err := testisolation.ExportGitHead(context.Background(), workerDir, head, fixedHead)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workerDir, "staged-only.txt"), []byte("staged blob"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, workerDir, "add", "staged-only.txt")
			index, err := testisolation.ExportGitIndex(context.Background(), workerDir, fixedHead)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			headPath, resultPath, ackPath := filepath.Join(root, "head.json"), filepath.Join(root, "result.json"), filepath.Join(root, "ack.json")
			final := isolatedStepEvent{Kind: "result", Outcome: &pipeline.StepOutcome{}, Index: index}
			if stepName == types.StepReview {
				final = isolatedStepEvent{Kind: "error", Error: pipeline.EncodeIsolatedTestError(pipeline.ErrAgentTimeout), Index: index}
			}
			for path, event := range map[string]isolatedStepEvent{headPath: {Kind: "head", Text: fixedHead, Pack: pack}, resultPath: final} {
				data, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sctx.Ctx = ctx
			cmd := exec.CommandContext(ctx, "sh", "-c", `IFS= read -r request || exit 11
cat "$1" || exit 12
IFS= read -r ack || exit 13
printf '%s' "$ack" > "$3"
cat "$2"`, "worker", headPath, resultPath, ackPath)
			outcome, retained, err := executeIsolatedWorker(sctx, cmd, "request", stepName, nil)
			if retained || stepName == types.StepReview && !errors.Is(err, pipeline.ErrAgentTimeout) || stepName != types.StepReview && (err != nil || outcome == nil) {
				t.Fatalf("handoff failed: %+v retained=%v %v", outcome, retained, err)
			}
			if data, err := os.ReadFile(ackPath); err != nil || string(data) != `""` {
				t.Fatalf("durable acknowledgement failed: %s %v", data, err)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || run.HeadSHA != fixedHead || gitCmd(t, dir, "rev-parse", "HEAD") != fixedHead {
				t.Fatalf("head not durably imported: %+v %v", run, err)
			}
			if got := gitCmd(t, dir, "show", ":staged-only.txt"); got != "staged blob" {
				t.Fatalf("index-only blob lost: %q", got)
			}
			rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, run.Branch)
			if err != nil {
				t.Fatal(err)
			}
			if stepName == types.StepReview {
				if rng == nil || rng.FromSHA != head || rng.ToSHA != fixedHead || rng.SourceRunID != run.ID {
					t.Fatalf("failed review lost host provenance: %+v", rng)
				}
			} else if rng != nil {
				t.Fatalf("non-review fix created review provenance: %+v", rng)
			}
		})
	}
}
