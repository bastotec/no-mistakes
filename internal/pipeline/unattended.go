package pipeline

import "github.com/kunchenguid/no-mistakes/internal/types"

// UnattendedMayApprove reports whether an unattended driver (axi --yes, the
// TUI's yolo mode) may approve or skip a gate parked on step. A CI-step gate
// only parks when the PR is not green - a failing check, a cancelled check no
// rerun replaces, a merge conflict, a review-bot failure, or checks held for
// approval - so approving it records passed-with-override. That override must
// come from an explicit operator response, never from standing --yes consent.
// Unattended drivers may still answer such a gate with fix.
func UnattendedMayApprove(step types.StepName) bool {
	return step != types.StepCI
}
