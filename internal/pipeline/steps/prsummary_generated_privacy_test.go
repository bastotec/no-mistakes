package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Representative, not a reconstruction of the missing rejected consumer body.
// Exercise the generated history boundary, not a validator keyword exception.
func TestGeneratedReviewHistoryBudgetPrivacy(t *testing.T) {
	const privateID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	description := "Automatic repair budget exhausted for review: consumed 2 repairs, current authority limit 2, configured automatic maximum 2. No further repair is authorized by the configured ceiling; approve or abort after reviewing the remaining findings (run " + privateID + ")."
	for _, category := range []string{"repair-budget", "repair-budget-hard-ceiling"} {
		original, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: "repair-budget-review", Category: category, Severity: types.FindingSeverityWarning, Action: types.ActionAskUser, Description: description}}})
		if err != nil {
			t.Fatal(err)
		}
		row := &db.StepResult{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &original}
		rounds := map[string][]*db.StepRound{"review": {{Round: 1, Trigger: "initial", FindingsJSON: &original}}}
		before, _ := json.Marshal(rounds)
		for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
			body, _ := BuildPipelineSummaryFor([]*db.StepResult{row}, rounds, testPipelineHeadSHA, provider)
			if strings.Contains(body, privateID) {
				t.Fatalf("generated budget identifier leaked: %s", body)
			}
			if !strings.Contains(body, "consumed 2 repairs") || !strings.Contains(body, "approve or abort") {
				t.Fatalf("budget/authority evidence masked: %s", body)
			}
		}
		after, _ := json.Marshal(rounds)
		if string(before) != string(after) || *row.FindingsJSON != original {
			t.Fatal("immutable evidence changed")
		}
	}
	// Same bytes in ordinary findings are literal evidence, not generated text.
	literal := types.Findings{Items: []types.Finding{{ID: "literal", Description: description}}}
	var b strings.Builder
	writeFindingItems(&b, &db.StepResult{StepName: types.StepReview}, &literal, prBodyHTML)
	if !strings.Contains(b.String(), privateID) {
		t.Fatal("literal finding was globally scrubbed")
	}
}
