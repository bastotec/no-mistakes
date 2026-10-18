package db

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsolatedCompletionOwnsPIDAndRollsBackAtomically(t *testing.T) {
	for _, point := range []string{"success", "cancelled", "different-owner", "terminal-write-fails", "ordinary"} {
		t.Run(point, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
				t.Fatal(err)
			}
			round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := d.ReserveStepRepairWithSelection(step.ID, round.ID, 1, nil)
			if err != nil || !decision.Granted {
				t.Fatalf("reserve: %+v %v", decision, err)
			}
			if err := d.ClaimStepRepair(step.ID, round.ID); err != nil {
				t.Fatal(err)
			}
			inv, err := d.RegisterRepairInvocation(step.ID, round.ID, 0, t.TempDir(), "deck", "isolated-test-step", 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.BindRepairInvocationProcess(inv.ID, "fixture", 12345); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			wantPID := 12345
			switch point {
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "different-owner":
				wantPID = 23456
				if err := d.SetStepAgentActivity(step.ID, "replacement", &wantPID); err != nil {
					t.Fatal(err)
				}
			case "terminal-write-fails":
				if _, err := d.sql.Exec(`CREATE TRIGGER reject_fixture_terminal BEFORE UPDATE OF state ON repair_invocations WHEN NEW.state = 'terminal' BEGIN SELECT RAISE(ABORT, 'fixture terminal write'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if point == "ordinary" {
				err = d.FinishRepairInvocation(inv.ID, []byte(`{}`), true, nil, nil)
			} else {
				err = d.FinishIsolatedTestRepairInvocation(ctx, inv.ID, []byte(`{}`), true, nil, nil)
			}
			success := point == "success" || point == "ordinary"
			if (err == nil) != success {
				t.Fatalf("finish: %v", err)
			}
			stored, err := d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			var state string
			if err := d.sql.QueryRow(`SELECT state FROM repair_invocations WHERE id=?`, inv.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if success && state != "terminal" || !success && state != "fixer_started" {
				t.Fatalf("wrong durable state %s", state)
			}
			if point == "success" {
				if stored.AgentPID != nil {
					t.Fatal("terminal result retained active PID")
				}
			} else if stored.AgentPID == nil || *stored.AgentPID != wantPID {
				t.Fatalf("lost active owner: %+v", stored.AgentPID)
			}
		})
	}
}
