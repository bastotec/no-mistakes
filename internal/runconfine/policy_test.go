package runconfine

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"strings"
	"testing"
)

func TestPolicyDigestBindsPolicyRunAndHead(t *testing.T) {
	policy := DefaultPolicy()
	base := PolicyDigest(policy, "run-1", "head-a")
	cases := []struct {
		name           string
		runID, headSHA string
		policy         Policy
		wantDifferent  bool
	}{
		{"same inputs", "run-1", "head-a", policy, false},
		{"different run", "run-2", "head-a", policy, true},
		{"different head", "run-1", "head-b", policy, true},
		{"different policy version", "run-1", "head-a", Policy{Version: 2, Scope: policy.Scope, Offline: true}, true},
		{"different policy scope", "run-1", "head-a", Policy{Version: 1, Scope: "test-only", Offline: true}, true},
		{"different policy posture", "run-1", "head-a", Policy{Version: 1, Scope: policy.Scope, Offline: false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PolicyDigest(tc.policy, tc.runID, tc.headSHA)
			if tc.wantDifferent && got == base {
				t.Fatal("digest collided across a boundary the pin must distinguish")
			}
			if !tc.wantDifferent && got != base {
				t.Fatal("digest is not deterministic for identical inputs")
			}
		})
	}
}

func TestValidateRefusesUnsupportedPolicies(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("default policy refused: %v", err)
	}
	for name, policy := range map[string]Policy{
		"future version": {Version: PolicyVersion + 1, Scope: "all-confinable-steps", Offline: true},
		"past version":   {Version: PolicyVersion - 1, Scope: "all-confinable-steps", Offline: true},
		"narrow scope":   {Version: PolicyVersion, Scope: "test-only", Offline: true},
		"online posture": {Version: PolicyVersion, Scope: "all-confinable-steps", Offline: false},
	} {
		t.Run(name, func(t *testing.T) {
			err := policy.Validate()
			if err == nil || !strings.Contains(err.Error(), "run-confinement-policy") {
				t.Fatalf("unsupported policy accepted: %v", err)
			}
		})
	}
}

func TestConfinableStepsAreClosed(t *testing.T) {
	want := []string{"review", "test", "document", "lint"}
	got := ConfinableSteps()
	if len(got) != len(want) {
		t.Fatalf("confinable steps changed: %v (scope changes bump PolicyVersion)", got)
	}
	for i, step := range got {
		if string(step) != want[i] {
			t.Fatalf("confinable steps changed: %v", got)
		}
	}
}

func TestCustomGatesAreConfinableIndependentlyOfAnchor(t *testing.T) {
	for _, anchor := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		if !IsConfinableStep(types.CustomGateStepName(anchor, "policy")) {
			t.Fatalf("gate after %s was excluded", anchor)
		}
	}
	for _, name := range []types.StepName{types.StepRebase, "gate.push.policy", "gate.rebase...", "gate.rebase."} {
		if IsConfinableStep(name) {
			t.Fatalf("invalid or excluded step %s accepted", name)
		}
	}
}
