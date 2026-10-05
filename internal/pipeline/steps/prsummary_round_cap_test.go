package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineSummary_ReviewRoundCapRendersUnresolvedNotes(t *testing.T) {
	t.Parallel()
	round1 := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":3,"description":"real bug"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"r"}`
	round2 := `{"findings":[{"id":"review-2","severity":"warning","file":"b.go","line":9,"description":"rename this helper"}],"summary":"1 issue","risk_level":"low","risk_rationale":"r"}`
	final := `{"findings":[],"summary":"1 issue","risk_level":"low","risk_rationale":"r","unresolved_notes":[{"id":"review-2","severity":"warning","file":"b.go","line":9,"description":"rename this helper"}],"round_cap":2}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &round1, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &round2, DurationMS: 600},
		},
	}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "⚠️ **Review** - 1 unresolved review note (round cap 2 reached)") {
		t.Fatalf("status line does not name the unresolved note:\n%s", md)
	}
	if strings.Contains(md, "**Review** - passed") || strings.Contains(md, "✅</summary>") {
		t.Fatalf("a capped review must not read as passed or fixed:\n%s", md)
	}
	if !strings.Contains(md, "Unresolved review notes (review.max_rounds 2 reached, not fixed):") || !strings.Contains(md, "rename this helper") {
		t.Fatalf("details do not list the unresolved notes:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewRoundCapApprovedOverBlockingFinding(t *testing.T) {
	t.Parallel()
	round1 := `{"findings":[{"id":"review-1","severity":"error","description":"real bug"},{"id":"review-2","severity":"info","description":"nit"}],"summary":"2 issues","risk_level":"high","risk_rationale":"r"}`
	final := `{"findings":[{"id":"review-1","severity":"error","description":"real bug"}],"summary":"2 issues","risk_level":"high","risk_rationale":"r","unresolved_notes":[{"id":"review-2","severity":"info","description":"nit"}],"round_cap":1}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &round1, DurationMS: 800}},
	}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "⚠️ **Review** - 1 error, 1 unresolved review note (round cap 1 reached)") {
		t.Fatalf("status line does not name both the approved error and the note:\n%s", md)
	}
}
