package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A clean rereview beside a carried unresolved error must not read as "no
// actionable finding survived": the PR validator refused that rationale as
// contradicting the error it then named (fleet-specialist-direct).
func TestReviewDispositionRationaleNeverCallsCarriedFindingsClean(t *testing.T) {
	carried := []types.Finding{{ID: "R1", Severity: "error", Action: types.ActionAskUser, Description: "Knows is missing on the phone"}}

	f := types.Findings{RiskLevel: "low", RiskRationale: types.NoActionableFindingRationale}
	appendReviewDispositionMetadata(&f, carried)
	if strings.Contains(f.RiskRationale, types.NoActionableFindingRationale) {
		t.Fatalf("rationale still claims nothing actionable survived: %q", f.RiskRationale)
	}
	if !strings.HasPrefix(f.RiskRationale, `carried error R1 (current validity not reverified): "Knows is missing on the phone"`) || !strings.HasSuffix(f.RiskRationale, "the latest rereview raised no new actionable finding") || f.RiskLevel != "high" {
		t.Fatalf("rationale = %q (risk %s), want the carried error first and the rereview scoped", f.RiskRationale, f.RiskLevel)
	}

	// An agent-written rationale is kept, scoped to the rereview.
	f = types.Findings{RiskLevel: "low", RiskRationale: "small, well-tested change"}
	appendReviewDispositionMetadata(&f, carried)
	if !strings.HasSuffix(f.RiskRationale, "; the latest rereview: small, well-tested change") {
		t.Fatalf("rationale = %q, want the rereview's own assessment scoped", f.RiskRationale)
	}

	// With nothing carried, the rereview's rationale is unchanged.
	f = types.Findings{RiskLevel: "low", RiskRationale: types.NoActionableFindingRationale}
	appendReviewDispositionMetadata(&f, nil)
	if f.RiskRationale != types.NoActionableFindingRationale {
		t.Fatalf("rationale = %q, want it unchanged", f.RiskRationale)
	}
}

// Exercise the serialized findings contract consumed by PR rendering, including
// the fix-only disposition and the boundary where nothing must be rewritten.
func TestReviewDispositionRationaleBoundaries(t *testing.T) {
	carried := []types.Finding{{ID: "R1", Severity: "error", Action: types.ActionAskUser, Description: "Missing phone action"}}
	fixed := []types.Finding{{ID: "R2", Severity: "error", Action: types.ActionAutoFix, Description: "Repair committed"}}
	const unresolved = `carried error R1 (current validity not reverified): "Missing phone action"`
	const uncertified = "fixes committed without rereview; their correctness is not certified"
	const clean = "; the latest rereview raised no new actionable finding"
	for _, tc := range []struct {
		name       string
		current    string
		unresolved []types.Finding
		fixed      []types.Finding
		want       string
		wantRisk   string
	}{
		{name: "carried with blank assessment", unresolved: carried, want: unresolved, wantRisk: "high"},
		{name: "case and whitespace clean assessment", current: "  NO ACTIONABLE FINDING SURVIVED VERIFICATION  ", unresolved: carried, want: unresolved + clean, wantRisk: "high"},
		{name: "agent assessment retained and scoped", current: "small change", unresolved: carried, want: unresolved + "; the latest rereview: small change", wantRisk: "high"},
		{name: "fix-only with clean assessment", current: types.NoActionableFindingRationale, fixed: fixed, want: uncertified + clean, wantRisk: "medium"},
		{name: "carried and fix-only together", current: types.NoActionableFindingRationale, unresolved: carried, fixed: fixed, want: unresolved + "; " + uncertified + clean, wantRisk: "high"},
		{name: "clean without dispositions unchanged", current: types.NoActionableFindingRationale, want: types.NoActionableFindingRationale, wantRisk: "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := types.Findings{RiskLevel: "low", RiskRationale: tc.current, ResolvedByFix: tc.fixed}
			appendReviewDispositionMetadata(&f, tc.unresolved)
			raw, err := types.MarshalFindingsJSON(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := types.ParseFindingsJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.RiskRationale != tc.want || got.RiskLevel != tc.wantRisk || got.RiskScope != types.FindingsRiskScopeSourceOrExternal || len(got.ResolvedByFix) != len(tc.fixed) {
				t.Fatalf("serialized disposition = %s; want rationale %q, risk %s", raw, tc.want, tc.wantRisk)
			}
			t.Logf("Generated findings: %s", raw)
		})
	}
}
