package custody

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// RecoveryRef keeps a terminal run's unpublished pipeline head reachable in
// the local gate until custody is explicitly returned.
func RecoveryRef(runID string) string {
	return "refs/no-mistakes/recover/" + runID
}

// PreserveRecoveryHead creates a run-specific recovery anchor without ever
// replacing existing evidence. A matching commit is idempotent; a conflicting
// or non-commit ref fails closed so reconciliation can inspect the original
// object.
func PreserveRecoveryHead(ctx context.Context, dir, runID, head string, gitOptions ...string) error {
	return PreserveRecoveryAnchor(ctx, dir, RecoveryRef(runID), head, gitOptions...)
}

func PreserveRecoveryAnchor(ctx context.Context, dir, ref, head string, gitOptions ...string) error {
	run := func(args ...string) (string, error) {
		return git.Run(ctx, dir, append(append([]string(nil), gitOptions...), args...)...)
	}
	if symbolic, err := run("symbolic-ref", "-q", ref); err == nil {
		return fmt.Errorf("recovery anchor %s is symbolic to %s instead of the verified commit %s", ref, symbolic, head)
	}
	if _, err := run("update-ref", "--no-deref", ref, head, strings.Repeat("0", len(head))); err == nil {
		return nil
	}
	if symbolic, err := run("symbolic-ref", "-q", ref); err == nil {
		return fmt.Errorf("recovery anchor %s is symbolic to %s instead of the verified commit %s", ref, symbolic, head)
	}
	existing, err := run("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("recovery anchor %s exists but is not the verified commit %s: %w", ref, head, err)
	}
	if existing != strings.TrimSpace(head) {
		return fmt.Errorf("recovery anchor %s conflicts: existing commit %s, verified commit %s", ref, existing, head)
	}
	return nil
}

// RecoveryLocalRef keeps the operator's pre-recovery head reachable when a
// guarded recovery adopts an equivalent rewritten pipeline head.
func RecoveryLocalRef(runID string) string {
	return "refs/no-mistakes/recover-local/" + runID
}

// RecoveryGateRef keeps an independently moved gate head reachable before a
// keep-local recovery changes the gate branch.
func RecoveryGateRef(runID string) string {
	return "refs/no-mistakes/recover-gate/" + runID
}
