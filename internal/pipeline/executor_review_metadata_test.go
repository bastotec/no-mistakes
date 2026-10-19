package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A rereview that reports nothing new merges back to the held findings; the
// result still carries the rereview's own level and unreviewed angles.
func TestWithReviewRoundMetadataKeepsTheFreshReviewsMetadata(t *testing.T) {
	held := `{"findings":[{"id":"held","severity":"warning","description":"deferred","action":"ask-user"}],"review_effort":"high"}`
	fresh := `{"findings":[],"review_effort":"xhigh","review_effort_reason":"large change","review_unreviewed":["security"]}`
	merged := withReviewRoundMetadata(mergeFindingsJSON(removeMatchingReviewHeldFindingsJSON(fresh, held), held), fresh)
	f, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != 1 || f.Items[0].ID != "held" || f.ReviewEffort != "xhigh" || len(f.ReviewUnreviewed) != 1 || f.ReviewUnreviewed[0] != "security" {
		t.Fatalf("merged = %+v", f)
	}
}

func TestReviewFinalDispositionDoesNotReuseRepairedClaim(t *testing.T) {
	for _, severity := range []string{"warning", "error"} {
		t.Run(severity, func(t *testing.T) {
			held, _ := types.MarshalFindingsJSON(types.Findings{
				Items:   []types.Finding{{ID: "held", Severity: severity, Description: "real surviving defect", Action: types.ActionAutoFix}},
				Summary: "repaired defect remains", RiskLevel: "high", RiskRationale: "repaired defect remains",
			})
			fresh := `{"findings":[],"summary":"fresh review complete","risk_level":"low","risk_rationale":"no new actionable findings","risk_scope":"source-or-external"}`
			merged := withReviewRoundMetadata(mergeFindingsJSON(removeMatchingReviewHeldFindingsJSON(fresh, held), held), fresh)
			f, err := types.ParseFindingsJSON(merged)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(f.Summary+f.RiskRationale, "repaired defect") || !strings.Contains(f.Summary, "fresh review complete") {
				t.Fatalf("stale metadata survived: %+v", f)
			}
			wantRisk := "medium"
			if severity == "error" {
				wantRisk = "high"
			}
			if f.RiskLevel != wantRisk || !strings.Contains(f.RiskRationale, "real surviving defect") || len(f.Items) != 1 {
				t.Fatalf("surviving defect understated: %+v", f)
			}
			// Follow the actual cap transition: the warning becomes a note,
			// while an error remains blocking. Neither is fixed by the merge.
			capped, blocking, notes, ok := capReviewFindingsJSON(merged, 3, severity == "error")
			if !ok || (severity == "warning" && (len(notes) != 1 || len(blocking) != 0)) || (severity == "error" && len(blocking) != 1) {
				t.Fatalf("bad cap disposition: %s", capped)
			}
		})
	}
}

func TestReviewFixOnlyMetadataDescribesFinalDisposition(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-op", true: "committed"}[committed], func(t *testing.T) {
			gate := `{"findings":[{"id":"fixed","severity":"error","description":"old defect","action":"auto-fix"}],"summary":"old defect remains","risk_level":"high","risk_rationale":"old defect remains","round_cap":3}`
			held := `{"findings":[{"id":"held","severity":"warning","description":"surviving warning","action":"ask-user"}]}`
			out := &StepOutcome{}
			(&Executor{}).resolveReviewFixWithoutRereview(out, gate, gate, held, committed, func(string) {})
			f, err := types.ParseFindingsJSON(out.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(f.Summary+f.RiskRationale, "old defect remains") || !strings.Contains(f.RiskRationale, "surviving warning") {
				t.Fatalf("stale metadata: %+v", f)
			}
			if committed {
				if len(f.ResolvedByFix) != 1 || len(f.UnresolvedNotes) != 1 || !strings.Contains(f.RiskRationale, "without rereview") || f.RiskLevel == "low" {
					t.Fatalf("unreviewed repair misrepresented: %+v", f)
				}
			} else if len(f.UnresolvedNotes) != 2 || len(f.ResolvedByFix) != 0 || f.RiskLevel != "high" || !strings.Contains(f.RiskRationale, "old defect") {
				t.Fatalf("no-op repair erased defect: %+v", f)
			}
		})
	}
}

func TestReviewDispositionPreservesFreshRiskAndCarriedNotes(t *testing.T) {
	fresh := `{"findings":[],"summary":"current source assessment","risk_level":"high","risk_rationale":"critical source surface"}`
	held := `{"findings":[{"id":"held","severity":"info","description":"remaining note","action":"no-op"}],"summary":"old assessment","risk_level":"high","risk_rationale":"old defect"}`
	merged := withReviewRoundMetadata(mergeFindingsJSON(removeMatchingReviewHeldFindingsJSON(fresh, held), held), fresh)
	f, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatal(err)
	}
	if f.RiskLevel != "high" || !strings.Contains(f.RiskRationale, "critical source surface") || strings.Contains(f.RiskRationale, "old defect") {
		t.Fatalf("fresh source risk lost: %+v", f)
	}
	carried := withCarriedReviewNotes(`{"findings":[],"summary":"clean rereview","risk_level":"low","risk_rationale":"no new findings"}`, []types.Finding{{ID: "warning", Severity: "warning", Description: "still open", Action: types.ActionAskUser}})
	f, err = types.ParseFindingsJSON(carried)
	if err != nil {
		t.Fatal(err)
	}
	if f.RiskLevel != "medium" || !strings.Contains(f.RiskRationale, "still open") || len(f.UnresolvedNotes) != 1 {
		t.Fatalf("carried note hidden: %+v", f)
	}
}
