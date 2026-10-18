package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
func TestTestStepOrdinaryRunKeepsAmbientEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX environment assertion")
	}
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "ordinary-sentinel")
	t.Setenv("NM_TEST_MODEL_URL", "")
	t.Setenv("NM_TEST_POSTGRES_ROOT", "")
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "ordinary", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ordinary","tested":["baseline"],"testing_summary":"baseline","artifacts":[],"scenarios":[{"name":"ordinary","result":"pass","live":true,"evidence":"fixture","reason":""}],"verdict":"go"}`)}, nil
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
		if err := json.NewEncoder(&wire).Encode(isolatedTestEvent{Kind: "error", Error: pipeline.EncodeIsolatedTestError(failure)}); err != nil {
			t.Fatal(err)
		}
		var received isolatedTestEvent
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
			if err := encoder.Encode(isolatedTestEvent{Kind: "invocation", Invocation: &inv}); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			if err := encoder.Encode(isolatedTestEvent{Kind: "activity"}); err != nil {
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
		var event isolatedTestEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := applyIsolatedTestObservation(sctx, event); err != nil {
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
