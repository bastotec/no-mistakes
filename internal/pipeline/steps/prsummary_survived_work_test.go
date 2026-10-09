package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRSummaryDoesNotPublishSurvivedWorkLocations(t *testing.T) {
	t.Parallel()
	raw, err := types.MarshalFindingsJSON(types.Findings{Summary: "repair interrupted", SurvivedWork: &types.SurvivedWork{Worktree: "/private/run-worktree", Files: []string{"?? private-file.txt"}, Next: "private handoff detail"}})
	if err != nil {
		t.Fatal(err)
	}
	results := []*db.StepResult{{ID: "ci", StepName: types.StepCI, Status: types.StepStatusAwaitingApproval, FindingsJSON: &raw}}
	rounds := map[string][]*db.StepRound{"ci": {{Round: 1, Trigger: "auto_fix", FindingsJSON: &raw}}}
	summary, _ := BuildPipelineSummary(results, rounds, testPipelineHeadSHA)
	attestation := buildPipelineAttestation(results, rounds, testPipelineHeadSHA)
	for _, rendered := range []string{summary, attestation} {
		for _, private := range []string{"/private/run-worktree", "private-file.txt", "private handoff detail", "survived_work"} {
			if strings.Contains(rendered, private) {
				t.Fatalf("local survival evidence published: %s", rendered)
			}
		}
	}
}
