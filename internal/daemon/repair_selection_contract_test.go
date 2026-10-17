package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the existing response boundary with fixture DB records only: no
// agent, service, or real validation run is launched by these tests.
type selectionContractStep struct {
	findings string
	repaired chan string
}

func (s *selectionContractStep) Name() types.StepName { return types.StepReview }
func (s *selectionContractStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if ctx.Fixing {
		s.repaired <- ctx.PreviousFindings
		return &pipeline.StepOutcome{}, nil
	}
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: s.findings}, nil
}

type selectionContractFixture struct {
	manager *RunManager
	db      *db.DB
	paths   *paths.Paths
	repo    *db.Repo
	global  *config.GlobalConfig
	policy  *config.RepoConfig
}

func newSelectionContractFixture(t *testing.T) *selectionContractFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepoWithID("selection-fixture", t.TempDir(), "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	return &selectionContractFixture{
		manager: NewRunManager(d, p, nil), db: d, paths: p, repo: repo,
		global: config.DefaultGlobalConfig(), policy: &config.RepoConfig{},
	}
}

func (f *selectionContractFixture) park(t *testing.T, findingID string) (*db.Run, *db.StepResult, <-chan string) {
	t.Helper()
	run, err := f.db.InsertRun(f.repo.ID, "feature-"+findingID, "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	step := &selectionContractStep{
		findings: fmt.Sprintf(`{"findings":[{"id":%q,"severity":"warning","file":"example.go","line":7,"description":"selected repair","action":"ask-user"},{"id":"unselected","severity":"info","description":"leave alone","action":"no-op"}]}`, findingID),
		repaired: make(chan string, 1),
	}
	executor := pipeline.NewExecutor(f.db, f.paths, config.Merge(f.global, f.policy), nil, []pipeline.Step{step}, nil)
	f.manager.executors[run.ID] = executor
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	workdir := t.TempDir()
	go func() { done <- executor.Execute(ctx, run, f.repo, workdir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("fixture executor did not stop")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		results, err := f.db.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.Status == types.StepStatusAwaitingApproval {
				return run, result, step.repaired
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fixture never parked")
	return nil, nil, nil
}

func TestRecordedRepairSelection_PersistsExactBindingAndFieldsAtZeroDefaults(t *testing.T) {
	f := newSelectionContractFixture(t)
	run, step, repaired := f.park(t, "R1")
	other, otherStep, _ := f.park(t, "R2")
	globalBefore, _ := json.Marshal(f.global)
	policyBefore, _ := json.Marshal(f.policy)

	if err := f.manager.HandleRespondWithOverrides(run.ID, types.StepReview, types.ActionFix, []string{"R1"}, map[string]string{"R1": "preserve the public API"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-repaired:
		findings, err := types.ParseFindingsJSON(raw)
		if err != nil || len(findings.Items) != 1 {
			t.Fatalf("repair payload = %s, %v", raw, err)
		}
		got := findings.Items[0]
		if got.ID != "R1" || got.File != "example.go" || got.Line != 7 || got.UserInstructions != "preserve the public API" {
			t.Fatalf("selection-to-field mapping changed: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("selected repair was not dispatched")
	}

	// Independent connection proves durable audit binding, not an in-memory response.
	audit, err := db.Open(f.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	storedStep, err := audit.GetStepResult(step.ID)
	if err != nil || storedStep.RunID != run.ID || storedStep.AutoFixLimit == nil || *storedStep.AutoFixLimit != 0 {
		t.Fatalf("recorded step binding/automatic limit = %+v, %v", storedStep, err)
	}
	rounds, err := audit.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("audit rounds = %+v, %v", rounds, err)
	}
	selected := rounds[0]
	if selected.SelectedFindingIDs == nil || *selected.SelectedFindingIDs != `["R1"]` || selected.SelectionSource == nil || *selected.SelectionSource != db.RoundSelectionSourceUser || selected.UserFindingsJSON == nil {
		t.Fatalf("persisted selection = %+v", selected)
	}
	payload, err := types.ParseFindingsJSON(*selected.UserFindingsJSON)
	if err != nil || len(payload.Items) != 1 || payload.Items[0].UserInstructions != "preserve the public API" {
		t.Fatalf("persisted selection fields = %+v, %v", payload, err)
	}
	otherRounds, err := audit.GetRoundsByStep(otherStep.ID)
	if err != nil || len(otherRounds) != 1 || otherRounds[0].SelectedFindingIDs != nil {
		t.Fatalf("selection leaked to run %s: %+v, %v", other.ID, otherRounds, err)
	}
	globalAfter, _ := json.Marshal(f.global)
	policyAfter, _ := json.Marshal(f.policy)
	if !reflect.DeepEqual(globalBefore, globalAfter) || !reflect.DeepEqual(policyBefore, policyAfter) {
		t.Fatal("explicit selection mutated global/repository configuration")
	}
	if config.Merge(f.global, &config.RepoConfig{}).AutoFix.Review != 0 || config.Merge(f.global, f.policy).AutoFix.Review != 0 {
		t.Fatal("selection raised global or repository automatic-review default")
	}
}

func TestRecordedRepairSelection_RefusesUnverifiedIdentityWithoutRecordingSelection(t *testing.T) {
	f := newSelectionContractFixture(t)
	run, step, _ := f.park(t, "R1")
	other, otherStep, _ := f.park(t, "R2")
	// A historical record is not an active gate or permission to repair it.
	terminal, err := f.db.InsertRun(f.repo.ID, "terminal", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(terminal.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, runID, findingID string }{
		{"different recorded run", other.ID, "R1"},
		{"terminal recorded run", terminal.ID, "R1"},
		{"different finding", run.ID, "R2"},
		{"unverifiable run", "missing-run", "R1"},
		{"unverifiable finding", run.ID, "missing-finding"},
		{"empty run", "", "R1"},
		{"empty finding", run.ID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.manager.HandleRespond(tc.runID, types.StepReview, types.ActionFix, []string{tc.findingID}); err == nil {
				t.Fatal("unverified selection accepted")
			}
			for _, id := range []string{step.ID, otherStep.ID} {
				rounds, err := f.db.GetRoundsByStep(id)
				if err != nil || len(rounds) != 1 || rounds[0].SelectedFindingIDs != nil || rounds[0].SelectionSource != nil {
					t.Fatalf("refused selection left an audit authorization: %+v, %v", rounds, err)
				}
			}
		})
	}
	// Refusals must not spend the valid selection's one explicit authority.
	if err := f.manager.HandleRespond(run.ID, types.StepReview, types.ActionFix, []string{"R1"}); err != nil {
		t.Fatalf("refusals consumed the valid repair: %v", err)
	}
}
