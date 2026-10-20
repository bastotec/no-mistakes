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
	if !strings.HasPrefix(f.RiskRationale, "unresolved error R1: Knows is missing on the phone") || !strings.HasSuffix(f.RiskRationale, "the latest rereview raised no new actionable finding") || f.RiskLevel != "high" {
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
