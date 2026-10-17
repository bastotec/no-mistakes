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

// A clean error-round rereview carries the stopped notes but no round_cap.
// The generated PR Markdown must still disclose them without inventing cap 0.
func TestBuildPipelineSummary_ReviewRoundCapNotesSurviveCleanRereview(t *testing.T) {
	t.Parallel()
	round1 := `{"findings":[{"id":"review-1","severity":"error","description":"real bug"},{"id":"review-2","severity":"warning","description":"rename this helper"},{"id":"review-3","severity":"info","description":"style suggestion"}],"summary":"3 issues","risk_level":"high","risk_rationale":"r"}`
	final := `{"findings":[],"summary":"clean rereview","risk_level":"low","risk_rationale":"fixed","unresolved_notes":[{"id":"review-2","severity":"warning","description":"rename this helper"},{"id":"review-3","severity":"info","description":"style suggestion"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &round1, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &final, DurationMS: 600},
		},
	}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "⚠️ **Review** - 2 unresolved review notes (round cap reached)") ||
		!strings.Contains(md, "Unresolved review notes (review.max_rounds reached, not fixed):") ||
		!strings.Contains(md, "rename this helper") || !strings.Contains(md, "style suggestion") {
		t.Fatalf("clean rereview lost the carried notes:\n%s", md)
	}
	if strings.Contains(md, "round cap 0") || strings.Contains(md, "review.max_rounds 0") || strings.Contains(md, "**Review** - passed") {
		t.Fatalf("clean rereview misrepresents the stopped notes:\n%s", md)
	}
	t.Logf("Generated PR Markdown:\n%s", md)
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

func TestBuildPipelineSummary_ReviewRoundCapFixWithoutRereview(t *testing.T) {
	t.Parallel()
	round1 := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":3,"description":"real bug"},{"id":"review-2","severity":"warning","description":"rename this helper"}],"summary":"2 issues","risk_level":"high","risk_rationale":"r"}`
	final := `{"findings":[],"summary":"2 issues","risk_level":"high","risk_rationale":"r","unresolved_notes":[{"id":"review-2","severity":"warning","description":"rename this helper"}],"resolved_by_fix":[{"id":"review-1","severity":"error","file":"a.go","line":3,"description":"real bug"}],"round_cap":1}`
	fixSummary := "changes applied"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &round1, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &final, FixSummary: &fixSummary, DurationMS: 600},
		},
	}

	md, risk := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "⚠️ **Review** - 1 finding fixed without rereview, 1 unresolved review note (round cap 1 reached)") {
		t.Fatalf("status line does not name the unreviewed fix and the note:\n%s", md)
	}
	if strings.Contains(md, "Re-checked") || strings.Contains(md, "✅</summary>") {
		t.Fatalf("a fix made at the round cap must not read as re-checked:\n%s", md)
	}
	if !strings.Contains(md, "Fixed without rereview (review.max_rounds 1 reached):") || !strings.Contains(md, "real bug") {
		t.Fatalf("details do not list the findings resolved by the fix:\n%s", md)
	}
	if !strings.Contains(md, "Unresolved review notes (review.max_rounds 1 reached, not fixed):") {
		t.Fatalf("details do not list the unresolved notes:\n%s", md)
	}
	if strings.Contains(md, "**Review** - passed") || strings.Contains(md, "no issues remain") {
		t.Fatalf("an unreviewed fix must not read as a clean verdict:\n%s", md)
	}
	history, dispositions, ok := strings.Cut(risk, "\n\nFinal recorded dispositions (not an independent clean verdict for the attested head):\n\n")
	if !ok || !strings.HasPrefix(history, "Historical review assessments (not current-behavior claims):\n\n") ||
		!strings.Contains(history, "- 🚨 High: r") {
		t.Fatalf("expected historical high risk without a new clean verdict, got: %q", risk)
	}
	unresolved, fixed, ok := strings.Cut(dispositions, "\nFixed without rereview (residual risk unverified):\n\n")
	if !ok || !strings.Contains(fixed, "real bug") {
		t.Fatalf("risk does not disclose the unverified fix: %q", dispositions)
	}
	if !strings.Contains(unresolved, "rename this helper") || strings.Contains(unresolved, "real bug") ||
		strings.Contains(unresolved, "No unresolved findings recorded.") {
		t.Fatalf("risk confuses repaired findings with unresolved notes: %q", unresolved)
	}
}
