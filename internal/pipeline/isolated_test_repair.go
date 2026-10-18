package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// RunIsolatedTestRepair gives the whole namespace worker the same durable
// custody as a native repair invocation. A terminal whole-step result replays
// without launching any product process; an interrupted started worker remains
// unresolved under the executor's existing reconciliation gate. It must never
// be reconstructed as a native agent invocation outside the namespace.
func (sctx *StepContext) RunIsolatedTestRepair(run func(markStarted func(int) error) (*StepOutcome, error)) (*StepOutcome, error) {
	round := ""
	if sctx.IsolatedRepairRound != nil {
		round = sctx.IsolatedRepairRound()
	}
	if !sctx.Fixing || round == "" {
		return run(nil)
	}
	invocation, err := sctx.DB.RegisterRepairInvocation(sctx.StepResultID, round, 0, sctx.WorkDir, "deck", "isolated-test-step", 0)
	if err != nil {
		return nil, fmt.Errorf("register isolated Test repair: %w", err)
	}
	if invocation.State == "terminal" {
		if invocation.ErrorText != nil {
			return nil, restoreRepairInvocationError(invocation.ErrorClass, *invocation.ErrorText)
		}
		var envelope agent.Result
		if err := json.Unmarshal(invocation.ResultJSON, &envelope); err != nil {
			return nil, err
		}
		var outcome StepOutcome
		if err := json.Unmarshal(envelope.Output, &outcome); err != nil {
			return nil, err
		}
		return &outcome, nil
	}
	started := false
	outcome, runErr := run(func(pid int) error {
		if err := sctx.DB.BindRepairInvocationProcess(invocation.ID, "isolated Test worker started", pid); err != nil {
			return err
		}
		started = true
		return nil
	})
	if !started {
		if err := sctx.DB.ResetUnstartedRepairInvocation(invocation.ID); err != nil {
			return nil, fmt.Errorf("%v (reset unstarted isolated Test repair: %w)", runErr, err)
		}
		return outcome, runErr
	}
	var resultJSON []byte
	if outcome != nil {
		output, err := json.Marshal(outcome)
		if err != nil {
			return nil, err
		}
		resultJSON, err = json.Marshal(&agent.Result{Output: output})
		if err != nil {
			return nil, err
		}
	}
	var class, message *string
	if runErr != nil {
		c, m := repairInvocationErrorClass(runErr), runErr.Error()
		class, message = &c, &m
	}
	if err := sctx.DB.FinishRepairInvocation(invocation.ID, resultJSON, outcome != nil, class, message); err != nil {
		return nil, fmt.Errorf("persist isolated Test repair result: %w", err)
	}
	return outcome, runErr
}
