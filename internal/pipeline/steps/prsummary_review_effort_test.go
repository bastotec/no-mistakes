package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineAttestation_CarriesTheReviewLevel(t *testing.T) {
	t.Parallel()
	review := `{"findings":[],"risk_level":"low","risk_rationale":"clean","review_effort":"high","review_effort_reason":"touches risky path internal/gate/x.go"}`
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &review},
		{ID: "push", StepName: types.StepPush, Status: types.StepStatusCompleted},
	}
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderUnknown, pipelineAttestationPolicy{})
	attestation := parsePipelineAttestationForTest(t, md)
	if attestation.Review == nil || attestation.Review.Effort != "high" || attestation.Review.Reason != "touches risky path internal/gate/x.go" {
		t.Fatalf("attestation review = %+v, want the chosen level and reason", attestation.Review)
	}

	// A review that recorded no level (an older run) attests none.
	plain := `{"findings":[],"risk_level":"low","risk_rationale":"clean"}`
	steps[0].FindingsJSON = &plain
	md, _ = buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderUnknown, pipelineAttestationPolicy{})
	if strings.Contains(md, `"review":{`) {
		t.Fatalf("attestation must omit review when none was recorded:\n%s", md)
	}
}

// A CI-repair restamp carries the old step statuses, so it carries the level
// those statuses were earned at; a rebind with this run's own steps does not
// borrow an older attestation's level.
func TestRebindPipelineAttestation_ReviewLevelFollowsTheSteps(t *testing.T) {
	t.Parallel()
	review := `{"findings":[],"risk_level":"low","risk_rationale":"clean","review_effort":"low","review_effort_reason":"small change: 1 files, 2 changed lines"}`
	steps := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &review}}
	body, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderUnknown, pipelineAttestationPolicy{})

	restamped, ok := rebindPipelineAttestationHead(body, strings.Repeat("b", 40))
	if !ok {
		t.Fatal("restamp failed")
	}
	if got := parsePipelineAttestationForTest(t, restamped).Review; got == nil || got.Effort != "low" {
		t.Fatalf("restamped review = %+v, want the carried level", got)
	}

	skipped := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusSkipped}}
	rebound, ok := rebindPipelineAttestationWithSteps(body, strings.Repeat("c", 40), skipped, pipelineAttestationPolicy{})
	if !ok {
		t.Fatal("rebind failed")
	}
	if got := parsePipelineAttestationForTest(t, rebound).Review; got != nil {
		t.Fatalf("rebind with this run's skipped review = %+v, want no level", got)
	}
}
