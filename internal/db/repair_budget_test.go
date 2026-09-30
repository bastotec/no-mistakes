package db

import (
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExplicitRepairAuthorizationStaysRecoverablyParkedUntilClaim(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","action":"ask-user"}]}`
	round, _ := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	ids := `["review-1"]`
	decision, err := d.AuthorizeStepRepair(step.ID, round.ID, 3, &ids, nil)
	if err != nil || !decision.Granted {
		t.Fatalf("AuthorizeStepRepair() = %+v, %v", decision, err)
	}
	parked, err := d.GetStepResult(step.ID)
	if err != nil || parked.Status != types.StepStatusAwaitingApproval || parked.FindingsJSON == nil {
		t.Fatalf("authorization erased recoverable gate: %+v, %v", parked, err)
	}
	storedRun, err := d.GetRun(run.ID)
	if err != nil || storedRun.AwaitingAgentSince == nil {
		t.Fatalf("authorization erased parked run marker: %+v, %v", storedRun, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := d.GetStepResult(step.ID)
	if claimed.Status != types.StepStatusFixing {
		t.Fatalf("claimed status = %s, want fixing", claimed.Status)
	}
	claimedRun, err := d.GetRun(run.ID)
	if err != nil || claimedRun.AwaitingAgentSince != nil {
		t.Fatalf("claimed repair retained parked marker: %+v, %v", claimedRun, err)
	}
	recoverable, err := d.HasRecoverableRepairDispatch(run.ID)
	if err != nil || !recoverable {
		t.Fatalf("claimed repair is not recoverable: %v, %v", recoverable, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatalf("replayed claim: %v", err)
	}
}

func TestStartedRepairRecoveryStaysFixingWithoutRedispatch(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.StartStepWithAutoFixLimit(step.ID, 2); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","action":"ask-user"}]}`
	round, _ := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	ids := `["review-1"]`
	if decision, err := d.AuthorizeStepRepair(step.ID, round.ID, 2, &ids, nil); err != nil || !decision.Granted {
		t.Fatalf("AuthorizeStepRepair() = %+v, %v", decision, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkStepRepairStarted(round.ID); err != nil {
		t.Fatal(err)
	}
	pid := 4242
	if err := d.SetStepAgentActivity(step.ID, "repair process active", &pid); err != nil {
		t.Fatal(err)
	}
	state, err := d.RestoreLegacyRepairAuthorization(step.ID, round.ID)
	if err != nil || state != "repair_started_unresolved" {
		t.Fatalf("restored state = %q, %v", state, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err == nil {
		t.Fatal("started repair was dispatchable again")
	}
	preserved, _ := d.GetStepResult(step.ID)
	if preserved.Status != types.StepStatusFixing {
		t.Fatalf("step status = %s, want fixing", preserved.Status)
	}
	if preserved.AgentPID == nil || *preserved.AgentPID != pid {
		t.Fatalf("agent pid = %v, want %d", preserved.AgentPID, pid)
	}
	storedRun, _ := d.GetRun(run.ID)
	if storedRun.AwaitingAgentSince != nil {
		t.Fatal("unresolved active repair was exposed as an approval gate")
	}
}

func TestClaimedRepairRecoveryNeverLaunchesUnboundInvocation(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","action":"ask-user"}]}`
	round, _ := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	ids := `["review-1"]`
	if decision, err := d.AuthorizeStepRepair(step.ID, round.ID, 1, &ids, nil); err != nil || !decision.Granted {
		t.Fatalf("AuthorizeStepRepair() = %+v, %v", decision, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatal(err)
	}
	state, err := d.RestoreLegacyRepairAuthorization(step.ID, round.ID)
	if err != nil || state != "repair_started_unresolved" {
		t.Fatalf("restored claimed state = %q, %v", state, err)
	}
	if err := d.ClaimStepRepair(step.ID, round.ID); err == nil {
		t.Fatal("claimed repair was dispatchable again")
	}
}

func TestRepairBudgetUsesPersistedStepLimitBeforeFirstReservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		grant bool
	}{
		{name: "lower lifecycle limit", limit: 1, grant: true},
		{name: "zero lifecycle limit", limit: 0, grant: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
			step, _ := d.InsertStepResult(run.ID, types.StepCI)
			if err := d.StartStepWithAutoFixLimit(step.ID, tc.limit); err != nil {
				t.Fatal(err)
			}
			if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
				t.Fatal(err)
			}
			if err := d.ResetStepsFrom(run.ID, types.StepCI.Order()); err != nil {
				t.Fatal(err)
			}
			if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
				t.Fatal(err)
			}
			observation, _ := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
			got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
			if err != nil || got.Granted != tc.grant || got.Limit != tc.limit || got.AuthorityLimit != tc.limit {
				t.Fatalf("reservation ignored persisted lifecycle limit: %+v %v", got, err)
			}
		})
	}
}

func TestRepairBudgetRevalidationPreservesReviewLifecycleLimit(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	review, _ := d.InsertStepResult(run.ID, types.StepReview)
	if _, err := d.InsertStepResult(run.ID, types.StepCI); err != nil {
		t.Fatal(err)
	}
	if err := d.StartStepWithAutoFixLimit(review.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(review.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.StartStepWithAutoFixLimit(review.ID, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.StartStepFixRound(review.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoFixLimit == nil || *got.AutoFixLimit != 1 {
		t.Fatalf("review lifecycle limit after revalidation = %v, want 1", got.AutoFixLimit)
	}
	observation, _ := d.InsertStepRound(review.ID, 1, "initial", nil, nil, 0)
	first, err := d.ReserveStepRepair(review.ID, observation.ID, 3, false)
	if err != nil || !first.Granted || first.Limit != 1 {
		t.Fatalf("first reservation after revalidation = %+v, %v", first, err)
	}
	next, _ := d.InsertStepRound(review.ID, 2, "auto_fix", nil, nil, 0)
	denied, err := d.ReserveStepRepair(review.ID, next.ID, 3, false)
	if err != nil || denied.Granted || denied.Limit != 1 || denied.Consumed != 1 {
		t.Fatalf("repeated reservation widened lifecycle limit: %+v, %v", denied, err)
	}
}

func TestRepairBudgetResetNormalizesLegacyExecutedSteps(t *testing.T) {
	for _, status := range []types.StepStatus{
		types.StepStatusRunning,
		types.StepStatusAwaitingApproval,
		types.StepStatusCompleted,
		types.StepStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
			step, _ := d.InsertStepResult(run.ID, types.StepReview)
			if err := d.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
				t.Fatal(err)
			}
			switch status {
			case types.StepStatusAwaitingApproval:
				if err := d.ParkStepForApproval(run.ID, step.ID, status, 1, 1, nil); err != nil {
					t.Fatal(err)
				}
			case types.StepStatusCompleted:
				if err := d.CompleteStep(step.ID, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			case types.StepStatusFailed:
				if err := d.FailStep(step.ID, "failed", 1); err != nil {
					t.Fatal(err)
				}
			}
			observation, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`INSERT INTO repair_budget_decisions(round_id, step_result_id, consumed, repair_limit, authority_limit, source) VALUES (?, ?, 1, 3, 3, 'user')`, observation.ID, step.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`UPDATE step_results SET auto_fix_limit = NULL, auto_fix_limit_provenance = 'legacy_unknown' WHERE id = ?`, step.ID); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
					t.Fatal(err)
				}
			}
			persisted, err := d.GetStepResult(step.ID)
			if err != nil || persisted.Status != types.StepStatusPending || persisted.AutoFixLimit == nil || *persisted.AutoFixLimit != 0 {
				t.Fatalf("reset legacy step = %+v, %v", persisted, err)
			}
			var provenance string
			var decisionLimit, authorityLimit int
			if err := d.sql.QueryRow(`SELECT auto_fix_limit_provenance FROM step_results WHERE id = ?`, step.ID).Scan(&provenance); err != nil {
				t.Fatal(err)
			}
			if err := d.sql.QueryRow(`SELECT repair_limit, authority_limit FROM repair_budget_decisions WHERE round_id = ?`, observation.ID).Scan(&decisionLimit, &authorityLimit); err != nil {
				t.Fatal(err)
			}
			if provenance != "initialized" || decisionLimit != 0 || authorityLimit != 0 {
				t.Fatalf("normalized lifecycle state = %q, limits %d/%d", provenance, decisionLimit, authorityLimit)
			}
			if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
				t.Fatal(err)
			}
			next, err := d.InsertStepRound(step.ID, 2, "initial", nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			denied, err := d.ReserveStepRepair(step.ID, next.ID, 3, true)
			if err != nil || denied.Granted || denied.Limit != 0 || denied.AuthorityLimit != 0 {
				t.Fatalf("reset legacy step regained authority: %+v, %v", denied, err)
			}
		})
	}
}

func TestRepairBudgetResetLeavesNeverStartedLegacyStepUninitialized(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
		t.Fatal(err)
	}
	persisted, err := d.GetStepResult(step.ID)
	if err != nil || persisted.AutoFixLimit == nil || *persisted.AutoFixLimit != 3 {
		t.Fatalf("never-started step limit = %+v, %v", persisted, err)
	}
	var provenance string
	if err := d.sql.QueryRow(`SELECT auto_fix_limit_provenance FROM step_results WHERE id = ?`, step.ID).Scan(&provenance); err != nil || provenance != "initialized" {
		t.Fatalf("never-started step provenance = %q, %v", provenance, err)
	}
	observation, _ := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	granted, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || !granted.Granted || granted.Limit != 3 {
		t.Fatalf("never-started step did not adopt configured limit: %+v, %v", granted, err)
	}
}

func TestRepairBudgetLoweredLimitCannotBeRestoredByExhaustedDecision(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		trigger := "auto_fix"
		if n == 1 {
			trigger = "initial"
		}
		observation, _ := d.InsertStepRound(step.ID, n, trigger, nil, nil, 0)
		got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
		if err != nil || !got.Granted {
			t.Fatalf("reserve repair %d: %+v %v", n, got, err)
		}
	}
	observation, _ := d.InsertStepRound(step.ID, 4, "auto_fix", nil, nil, 0)
	if got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false); err != nil || got.Granted {
		t.Fatalf("expected exhaustion decision: %+v %v", got, err)
	}
	lowered, err := d.ReserveStepRepair(step.ID, observation.ID, 1, true)
	if err != nil || lowered.Granted || lowered.Limit != 1 || lowered.AuthorityLimit != 1 {
		t.Fatalf("exhausted decision restored larger limit: %+v %v", lowered, err)
	}
	retried, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
	if err != nil || retried.Granted || retried.Limit != 1 || retried.AuthorityLimit != 1 {
		t.Fatalf("retry widened lowered limit: %+v %v", retried, err)
	}
}

func TestRepairBudgetResponsePinsAndExtendsAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepCI)
	observation, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
	if err != nil || !first.Granted || first.Consumed != 1 || first.AuthorityLimit != 1 || first.Limit != 3 {
		t.Fatalf("first response did not pin one repair: %+v %v", first, err)
	}
	next, _ := d.InsertStepRound(step.ID, 2, "auto_fix", nil, nil, 0)
	denied, err := d.ReserveStepRepair(step.ID, next.ID, 3, false)
	if err != nil || denied.Granted || denied.Consumed != 1 || denied.AuthorityLimit != 1 || denied.Limit != 3 {
		t.Fatalf("automatic repair exceeded response authority: %+v %v", denied, err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}

	var grants atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := d.ReserveStepRepair(step.ID, next.ID, 3, true)
			if err != nil {
				t.Error(err)
				return
			}
			if got.Granted {
				grants.Add(1)
			}
		}()
	}
	wg.Wait()
	if grants.Load() != 1 {
		t.Fatalf("later response launched %d repairs, want one", grants.Load())
	}
	afterGrant, _ := d.InsertStepRound(step.ID, 3, "auto_fix", nil, nil, 0)
	got, err := d.ReserveStepRepair(step.ID, afterGrant.ID, 100, false)
	if err != nil || got.Granted || got.Consumed != 2 || got.AuthorityLimit != 2 || got.Limit != 3 {
		t.Fatalf("retry or config reload widened response authority: %+v %v", got, err)
	}
}

func TestRepairBudgetLegacyNullActiveLimitStaysAutomaticZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-null.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepCI)
	if err := d.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-red","severity":"error","description":"check failed","action":"auto-fix"}],"summary":"check failed"}`
	observation, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE step_results SET auto_fix_limit = NULL, auto_fix_limit_provenance = 'legacy_unknown' WHERE id = ?`, step.ID); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || denied.Granted || denied.Limit != 0 || denied.AuthorityLimit != 0 || !denied.ExplicitRepairAvailable {
		t.Fatalf("legacy NULL gained automatic authority or hid explicit authority: %+v %v", denied, err)
	}
	persisted, err := d.GetStepResult(step.ID)
	if err != nil || persisted.AutoFixLimit == nil || *persisted.AutoFixLimit != 0 {
		t.Fatalf("legacy NULL was not normalized to zero: %+v %v", persisted, err)
	}
	var provenance string
	if err := d.sql.QueryRow(`SELECT auto_fix_limit_provenance FROM step_results WHERE id = ?`, step.ID).Scan(&provenance); err != nil || provenance != "legacy_normalized" {
		t.Fatalf("legacy NULL provenance after normalization = %q, %v", provenance, err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	retriedBeforeGrant, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || retriedBeforeGrant.Granted || !retriedBeforeGrant.ExplicitRepairAvailable {
		t.Fatalf("resume hid one-time legacy authority: %+v %v", retriedBeforeGrant, err)
	}
	granted, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
	if err != nil || !granted.Granted || granted.Consumed != 1 || granted.Limit != 0 || granted.AuthorityLimit != 1 || granted.ExplicitRepairAvailable {
		t.Fatalf("explicit response did not grant exactly one repair: %+v %v", granted, err)
	}
	persisted, err = d.GetStepResult(step.ID)
	if err != nil || persisted.AutoFixLimit == nil || *persisted.AutoFixLimit != 0 {
		t.Fatalf("explicit response widened automatic limit: %+v %v", persisted, err)
	}
	if err := d.sql.QueryRow(`SELECT auto_fix_limit_provenance FROM step_results WHERE id = ?`, step.ID).Scan(&provenance); err != nil || provenance != "initialized" {
		t.Fatalf("legacy authority was not consumed exactly once: %q, %v", provenance, err)
	}
	next, err := d.InsertStepRound(step.ID, 2, "user_fix", &findings, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for range 2 {
		retried, err := d.ReserveStepRepair(step.ID, next.ID, 3, false)
		if err != nil || retried.Granted || retried.Consumed != 1 || retried.Limit != 0 || retried.AuthorityLimit != 0 || retried.ExplicitRepairAvailable {
			t.Fatalf("resume or retry widened legacy authority: %+v %v", retried, err)
		}
	}
}

func TestRepairBudgetInitializedZeroCannotReopenPriorAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initialized-zero.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.StartStepWithAutoFixLimit(step.ID, 3); err != nil {
		t.Fatal(err)
	}
	firstRound, _ := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	first, err := d.ReserveStepRepair(step.ID, firstRound.ID, 3, false)
	if err != nil || !first.Granted {
		t.Fatalf("initial reservation: %+v %v", first, err)
	}
	if err := d.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
		t.Fatal(err)
	}
	observation, _ := d.InsertStepRound(step.ID, 2, "auto_fix", nil, nil, 0)
	denied, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
	if err != nil || denied.Granted || denied.Limit != 0 || denied.AuthorityLimit != 0 || denied.Consumed != 1 {
		t.Fatalf("initialized zero reopened prior authority: %+v %v", denied, err)
	}
	var provenance string
	var decisionLimit, decisionAuthority int
	if err := d.sql.QueryRow(`SELECT auto_fix_limit_provenance FROM step_results WHERE id = ?`, step.ID).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT MAX(repair_limit), MAX(authority_limit) FROM repair_budget_decisions WHERE step_result_id = ?`, step.ID).Scan(&decisionLimit, &decisionAuthority); err != nil {
		t.Fatal(err)
	}
	if provenance != "initialized" || decisionLimit != 0 || decisionAuthority != 0 {
		t.Fatalf("lowered lifecycle state = %q, limits %d/%d", provenance, decisionLimit, decisionAuthority)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for range 2 {
		retried, err := d.ReserveStepRepair(step.ID, observation.ID, 3, true)
		if err != nil || retried.Granted || retried.Limit != 0 || retried.AuthorityLimit != 0 || retried.Consumed != 1 {
			t.Fatalf("resume or retry reopened initialized zero: %+v %v", retried, err)
		}
	}
}

func TestRepairBudgetLegacyOverCeilingSurvivesMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	for n := 1; n <= 5; n++ {
		trigger := "initial"
		if n > 1 {
			trigger = "user_fix"
		}
		if _, err := d.InsertStepRound(step.ID, n, trigger, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Model an existing schema with no reservation table, then reopen/migrate.
	if _, err := d.sql.Exec(`DROP TABLE repair_budget_decisions`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) != 5 {
		t.Fatalf("legacy records lost: %v %v", rounds, err)
	}
	last := rounds[4]
	got, err := d.ReserveStepRepair(step.ID, last.ID, 3, false)
	if err != nil || got.Granted || got.Consumed != 4 {
		t.Fatalf("legacy overrun granted: %+v %v", got, err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err = d.ReserveStepRepair(step.ID, last.ID, 100, false)
	if err != nil || got.Granted || got.Consumed != 4 || got.Limit != 3 {
		t.Fatalf("restart minted authority: %+v %v", got, err)
	}
	preserved, _ := d.GetRoundsByStep(step.ID)
	if len(preserved) != len(rounds) {
		t.Fatal("restart reset legacy rounds")
	}
	for n := range rounds {
		if !reflect.DeepEqual(preserved[n], rounds[n]) {
			t.Fatal("migration changed a legacy round")
		}
	}
}

func TestRepairBudgetAutomaticSequenceUsesConfiguredLimit(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	for n := 1; n <= 3; n++ {
		trigger := "auto_fix"
		if n == 1 {
			trigger = "initial"
		}
		observation, _ := d.InsertStepRound(step.ID, n, trigger, nil, nil, 0)
		got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
		if err != nil || !got.Granted || got.Consumed != n || got.AuthorityLimit != 3 {
			t.Fatalf("automatic repair %d: %+v %v", n, got, err)
		}
	}
	observation, _ := d.InsertStepRound(step.ID, 4, "auto_fix", nil, nil, 0)
	got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || got.Granted || got.Consumed != 3 {
		t.Fatalf("automatic sequence exceeded configured limit: %+v %v", got, err)
	}
}

func TestRepairBudgetOneRemainingLaunchesTwoOfThree(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	// The sole counterfactual is consumed=1 instead of consumed=3.
	d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	observation, _ := d.InsertStepRound(step.ID, 2, "user_fix", nil, nil, 0)
	got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || !got.Granted || got.Consumed != 2 || got.Limit != 3 {
		t.Fatalf("2/3 repair refused: %+v %v", got, err)
	}
	retry, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || retry.Granted || !retry.Duplicate {
		t.Fatalf("retry launched duplicate: %+v %v", retry, err)
	}
}

func TestRepairBudgetInterruptedLegacySelectionsRemainConsumed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepCI)
	ids := `["ci-red"]`
	for n := 1; n <= 3; n++ {
		observation, err := d.InsertStepRound(step.ID, n, "initial", nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetStepRoundSelection(observation.ID, &ids, RoundSelectionSourceUser); err != nil {
			t.Fatal(err)
		}
	}
	// Recovery re-observes checks without a completed fixer result. Each pending
	// selection still represents spent authority; an initial trigger is no reset.
	observation, _ := d.InsertStepRound(step.ID, 4, "initial", nil, nil, 0)
	got, err := d.ReserveStepRepair(step.ID, observation.ID, 3, false)
	if err != nil || got.Granted || got.Consumed != 3 {
		t.Fatalf("interrupted authority discarded: %+v %v", got, err)
	}
}
