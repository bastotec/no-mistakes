// Package runconfine owns the versioned per-run execution capability boundary.
//
// A confined run binds preparation, Review, Test, Document, Lint and repository
// gates, together with their repair rounds, to one OS-enforced isolation policy.
// The policy is pinned against the run's identity before pipeline execution and
// validated before each worker dispatch. Steps outside that scope skip with an
// explicit reason; publication also refuses at its shared boundary instead of
// falling back to daemon credentials. Daemon-owned launch and recovery reads
// remain outside the worker boundary.
//
// This is deliberately a separate pin from Test-only isolation
// (run_test_isolation, `--isolate-test`): that opt-in confines the whole Test
// step alone. A confined run reuses the same worker machinery but binds every
// confinable step, so the two pins must never be conflated.
package runconfine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PolicyVersion is the wire/contract version of the confinement policy. It is
// part of the policy digest, so a policy change is a different digest, not a
// silent reinterpretation of an existing pin.
const PolicyVersion = 1

// Policy is the complete, closed description of what a confined run may do.
// It is built by the daemon from launch inputs and the confinement substrate
// check - never from repository or global configuration, which a pushed branch
// must not be able to steer.
type Policy struct {
	// Version is PolicyVersion at pin time.
	Version int `json:"version"`
	// Scope names which executions the boundary covers. Today exactly
	// "all-confinable-steps": every step in ConfinableSteps plus repository
	// gates, their repair rounds and preparation. A future narrower scope is
	// a new version, never a reinterpretation.
	Scope string `json:"scope"`
	// Offline marks the deny-by-default network posture: no forge, provider or
	// arbitrary egress from confined executions, and daemon-side publication,
	// PR and CI adapters do not run on behalf of the run. Publication also
	// refuses at pipeline.RefuseConfinedPublication.
	Offline bool `json:"offline"`
}

// Canonical is the policy's stable serialization; PolicyDigest hashes it.
func (p Policy) Canonical() []byte {
	return []byte(fmt.Sprintf("v%d|%s|%t", p.Version, p.Scope, p.Offline))
}

// PolicyDigest binds a policy to one run identity and immutable submitted head.
// It is persisted at launch and re-derived before worker dispatch, including
// after recovery. A repair may advance the current head without changing the pin.
func PolicyDigest(p Policy, runID, headSHA string) string {
	sum := sha256.Sum256(append(p.Canonical(), []byte("|"+runID+"|"+headSHA)...))
	return hex.EncodeToString(sum[:])
}

// DefaultPolicy is the only policy the daemon currently issues. The scope is
// closed: adding or removing a confinable step is a policy change and bumps
// PolicyVersion, because an existing pin would otherwise bind a boundary the
// new policy does not provide.
func DefaultPolicy() Policy {
	return Policy{Version: PolicyVersion, Scope: "all-confinable-steps", Offline: true}
}

// Validate refuses a policy this binary cannot honor, before any side effect.
func (p Policy) Validate() error {
	if p.Version != PolicyVersion {
		return fmt.Errorf("run-confinement-policy: unsupported policy version %d (supported %d)", p.Version, PolicyVersion)
	}
	if p.Scope != "all-confinable-steps" {
		return fmt.Errorf("run-confinement-policy: unsupported scope %q", p.Scope)
	}
	if !p.Offline {
		// The offline posture is what makes daemon-side publication refusal
		// sound; an online variant is future authorization, never implicit.
		return fmt.Errorf("run-confinement-policy: only the offline capability policy is supported")
	}
	return nil
}

// ConfinableSteps lists the core steps whose whole execution - initial round,
// fix rounds and preparation - runs inside the boundary. IsConfinableStep also
// admits repository gates independently of their anchor's core-step policy.
//
// Deliberately absent, each for its own reason:
//   - push, pr, ci: their daemon-side forge adapters are exactly what the
//     offline policy forbids, so those steps refuse (skip with an explicit
//     reason) instead of executing, and any later narrow external capability
//     for them is separate authorization, never implicit.
//   - jev: it is advisory-only and its only action is a gateway call, so under
//     the offline policy it skips with a reason rather than running anywhere.
//   - intent: a confined launch requires an explicit --intent, so no intent
//     inference is needed and the step skips.
//   - rebase: its integration fetch is forge egress; a confined run validates
//     the submitted head as-is, so the step skips without integration.
func ConfinableSteps() []types.StepName {
	return []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint}
}

// IsConfinableStep reports whether step's whole execution belongs inside the
// boundary.
func IsConfinableStep(step types.StepName) bool {
	if step.IsCustomGate() {
		return true
	}
	return sliceContains(ConfinableSteps(), step)
}

func sliceContains(steps []types.StepName, step types.StepName) bool {
	for _, candidate := range steps {
		if candidate == step {
			return true
		}
	}
	return false
}
