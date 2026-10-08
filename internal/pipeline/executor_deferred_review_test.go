package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ReviewPartialResponsePreservesDeferredFinding(t *testing.T) {
	for _, tc := range []struct {
		name           string
		single         bool
		rereported     bool
		sameIDReworded bool
	}{
		{name: "multi-finding"},
		{name: "single-held-with-added-repair", single: true},
		{name: "rereported-held-finding", rereported: true},
		{name: "rereported-held-finding-with-same-id", rereported: true, sameIDReworded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			held := types.Finding{ID: "held", Severity: types.FindingSeverityWarning, Description: "authorized follow-up", Action: types.ActionAutoFix, UserInstructions: "implement the authorized follow-up"}
			answered := types.Finding{ID: "answered", Severity: types.FindingSeverityError, Description: "repair this", Action: types.ActionAutoFix}
			items := []types.Finding{answered, held}
			if tc.single {
				items = []types.Finding{held}
			}
			raw, err := types.MarshalFindingsJSON(types.Findings{Items: items})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if calls == 1 {
					return &StepOutcome{NeedsApproval: true, Findings: raw}, nil
				}
				if calls == 2 && tc.rereported {
					duplicate := held
					if tc.sameIDReworded {
						duplicate.Description = "freshly worded authorized follow-up"
					} else {
						duplicate.ID = "fresh-review-id"
					}
					duplicate.Action = types.ActionAskUser
					duplicate.UserInstructions = ""
					reported, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{duplicate}})
					return &StepOutcome{Findings: reported, ReviewApprovedHeadSHA: run.HeadSHA}, err
				}
				return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			workDir := t.TempDir()
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			var responseErr error
			if tc.single {
				responseErr = exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, nil, []types.Finding{answered})
			} else {
				responseErr = exec.Respond(types.StepReview, types.ActionFix, []string{"answered"})
			}
			if responseErr != nil {
				t.Fatal(responseErr)
			}
			deadline := time.After(5 * time.Second)
			for {
				results, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if results[0].Status == types.StepStatusCompleted {
					t.Fatal("regenerated gate silently completed, dropping deferred finding")
				}
				if results[0].Status == types.StepStatusFixReview {
					if results[0].FindingsJSON == nil {
						t.Fatal("missing regenerated findings")
					}
					findings, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
					if err != nil || len(findings.Items) != 1 || findings.Items[0] != held {
						t.Fatalf("held finding changed or lost: %+v, %v", findings, err)
					}
					rounds, err := database.GetRoundsByStep(results[0].ID)
					if err != nil || len(rounds) != 2 || rounds[1].FindingsJSON == nil || *rounds[1].FindingsJSON != *results[0].FindingsJSON {
						t.Fatalf("regenerated gate is not durable: %+v, %v", rounds, err)
					}
					parked, err := database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateRecoveredRun(database, parked, []Step{step}); err != nil {
						t.Fatalf("regenerated gate cannot recover: %v", err)
					}
					if err := exec.Respond(types.StepReview, types.ActionFix, []string{"held"}); err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("fully answered gate did not finish")
					}
					return
				}
				select {
				case <-deadline:
					t.Fatal("regenerated gate did not park")
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
}

// Selecting the only finding (or every finding) must not manufacture a held
// gate: ordinary progress still finishes after the clean rereview.
func TestExecutor_ReviewFullyAnsweredResponseCompletes(t *testing.T) {
	for _, ids := range [][]string{{"only"}, {"first", "second"}} {
		t.Run(ids[0], func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			items := make([]types.Finding, len(ids))
			for i, id := range ids {
				items[i] = types.Finding{ID: id, Severity: types.FindingSeverityWarning, Description: id, Action: types.ActionAutoFix}
			}
			raw, err := types.MarshalFindingsJSON(types.Findings{Items: items})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if calls == 1 {
					return &StepOutcome{NeedsApproval: true, Findings: raw}, nil
				}
				if sctx.DeferredFindings != "" {
					t.Errorf("fully answered gate has deferred findings: %s", sctx.DeferredFindings)
				}
				return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			workDir := t.TempDir()
			done := make(chan error, 1)
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if err := exec.Respond(types.StepReview, types.ActionFix, ids); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fully answered gate did not finish")
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if results[0].Status != types.StepStatusCompleted || results[0].FindingsJSON != nil {
				t.Fatalf("fully answered gate changed behavior: %+v", results[0])
			}
			if calls != 2 {
				t.Fatalf("review calls = %d, want 2", calls)
			}
		})
	}
}
