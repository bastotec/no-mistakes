package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestUnattendedMayApprove(t *testing.T) {
	for _, tc := range []struct {
		step types.StepName
		want bool
	}{
		{types.StepCI, false},
		{types.StepName("babysit"), false},
		{types.StepReview, true},
	} {
		t.Run(string(tc.step), func(t *testing.T) {
			if got := UnattendedMayApprove(tc.step); got != tc.want {
				t.Fatalf("UnattendedMayApprove(%q) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}
