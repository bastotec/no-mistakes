package db

import (
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
