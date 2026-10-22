package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Synthetic legacy receipt shape; no production run data belongs in this fixture.
const legacyDuplicateTestGate = `{"findings":[{"id":"test-2","severity":"info","action":"no-op","description":"completed observation"},{"id":"test-2","severity":"warning","action":"ask-user","description":"authorized repair"},{"id":"test-3","severity":"info","action":"no-op","description":"other observation"}],"summary":"legacy gate"}`

func TestLegacyDuplicateFindingSelection(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ids       []string
		wantErr   string
		wantItems int
	}{
		{"legacy", legacyDuplicateTestGate, []string{"test-2"}, "", 1},
		{"actionable first", `{"findings":[{"id":"x","action":"auto-fix"},{"id":"x","action":"no-op"}]}`, []string{"x"}, "", 1},
		{"legacy missing action", `{"findings":[{"id":"x","action":"no-op"},{"id":"x"}]}`, []string{"x"}, "", 1},
		{"no-op only unchanged", legacyDuplicateTestGate, []string{"test-3"}, "", 1},
		{"unknown", legacyDuplicateTestGate, []string{"test-4"}, "not present", 0},
		{"empty", legacyDuplicateTestGate, []string{""}, "must not be empty", 0},
		{"none", legacyDuplicateTestGate, nil, "selected no findings", 0},
		{"repeated", legacyDuplicateTestGate, []string{"test-2", "test-2"}, "more than once", 0},
		{"two actionable", `{"findings":[{"id":"x","action":"no-op"},{"id":"x","action":"ask-user"},{"id":"x","action":"auto-fix"}]}`, []string{"x"}, "ambiguous", 0},
		{"missing action is actionable", `{"findings":[{"id":"x"},{"id":"x","action":"ask-user"}]}`, []string{"x"}, "ambiguous", 0},
		{"duplicate no-ops unchanged", `{"findings":[{"id":"x","action":"no-op"},{"id":"x","action":"no-op"}]}`, []string{"x"}, "ambiguous", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFindingSelection(tc.raw, tc.ids, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := types.ParseFindingsJSON(filterFindingsJSON(tc.raw, tc.ids))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != tc.wantItems {
				t.Fatalf("selected = %+v", got)
			}
			if tc.name == "legacy" && got.Items[0].Description != "authorized repair" {
				t.Fatalf("wrong row: %+v", got.Items)
			}
		})
	}
	if err := validateFindingSelection(legacyDuplicateTestGate, nil, []types.Finding{{Description: "user addition"}}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutor_ResumeLegacyDuplicateTestGate(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		name := "parked"
		if authorized {
			name = "authorized before restart"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			sr, err := database.InsertStepResult(run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStepWithAutoFixLimit(sr.ID, 1); err != nil {
				t.Fatal(err)
			}
			raw := legacyDuplicateTestGate
			round, err := database.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusFixReview, 1, 1, &raw); err != nil {
				t.Fatal(err)
			}
			if authorized {
				ids := `["test-2"]`
				decision, err := database.AuthorizeStepRepair(sr.ID, round.ID, 1, &ids, nil)
				if err != nil || !decision.Granted {
					t.Fatalf("authorize: %+v %v", decision, err)
				}
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				got, err := types.ParseFindingsJSON(sctx.PreviousFindings)
				if err != nil {
					return nil, err
				}
				if !sctx.Fixing || len(got.Items) != 1 || got.Items[0].Description != "authorized repair" || got.Items[0].Action != types.ActionAskUser {
					return nil, fmt.Errorf("repair payload: %s", sctx.PreviousFindings)
				}
				if !authorized && got.Items[0].UserInstructions != "focused repair" {
					return nil, fmt.Errorf("instructions missing: %+v", got.Items)
				}
				return &StepOutcome{}, nil
			}}
			exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			workDir := t.TempDir()
			go func() { done <- exec.Resume(ctx, run, repo, workDir) }()
			if !authorized {
				deadline := time.Now().Add(5 * time.Second)
				for {
					exec.mu.Lock()
					waiting := exec.waiting
					exec.mu.Unlock()
					if waiting {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("gate not ready")
					}
					time.Sleep(time.Millisecond)
				}
				err := exec.RespondWithOverrides(types.StepTest, types.ActionFix, []string{"test-2"}, map[string]string{"test-2": "focused repair"}, nil)
				if err != nil {
					cancel()
					<-done
					t.Fatal(err)
				}
			}
			waitExecutorDone(t, done)
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			rounds, err := database.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != raw {
				t.Fatal("original findings changed")
			}
			if rounds[0].SelectedFindingIDs == nil {
				t.Fatal("missing decision")
			}
			if *rounds[0].SelectedFindingIDs != `["test-2"]` {
				t.Fatalf("selected IDs: %s", *rounds[0].SelectedFindingIDs)
			}
		})
	}
}
