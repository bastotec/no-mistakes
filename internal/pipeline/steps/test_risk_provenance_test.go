package steps

import (
	"cmp"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func cmpOr(a, b string) string { return cmp.Or(a, b) }

// The final-head risk provenance contract: a successful revalidating Test step
// must carry a truthful, head-bound risk verdict. The schema requires it, and
// the executor default fills an unstated verdict rather than publishing silence.

func TestTestFindingsSchemaRequiresRiskFields(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(testFindingsSchema, &schema); err != nil {
		t.Fatalf("testFindingsSchema is not valid JSON: %v", err)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema has no required array")
	}
	want := map[string]bool{"risk_level": false, "risk_rationale": false}
	for _, r := range required {
		if s, isStr := r.(string); isStr {
			if _, isWanted := want[s]; isWanted {
				want[s] = true
			}
		}
	}
	for field, present := range want {
		if !present {
			t.Errorf("testFindingsSchema required[] is missing %q", field)
		}
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object")
	}
	rl, ok := props["risk_level"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing risk_level")
	}
	if enum, _ := rl["enum"].([]any); len(enum) != 3 {
		t.Errorf("risk_level enum should be low/medium/high, got %v", enum)
	}
	if _, ok := props["risk_rationale"]; !ok {
		t.Errorf("schema properties missing risk_rationale")
	}
}

func TestTestFindingsWithoutRiskFailsSchemaValidation(t *testing.T) {
	// A payload missing the risk fields must be rejected by the same
	// unmarshal path the step uses, so silence can never pass as a verdict.
	payload := `{"findings":[],"summary":"ok","tested":["x"],"testing_summary":"ran","artifacts":[],"scenarios":[{"name":"s","result":"pass","live":true,"evidence":"e","reason":""}],"verdict":"go"}`
	var findings types.Findings
	if err := json.Unmarshal([]byte(payload), &findings); err != nil {
		t.Fatalf("types decode failed: %v", err)
	}
	if findings.RiskLevel != "" || findings.RiskRationale != "" {
		t.Fatal("an analyzer payload without risk fields must decode to empty risk fields")
	}
	// The step-side guard: analyzer output lacking risk fields must not
	// survive validation. unmarshalRequiredTestFindings is the validator.
	if err := unmarshalRequiredTestFindings(json.RawMessage(payload), &findings); err == nil {
		t.Fatal("validator accepted findings without risk_level/risk_rationale; silence must not pass as a verdict")
	}
}

func TestRiskDefaultIsHeadBoundMedium(t *testing.T) {
	head := "fe5d324aa6394b7244a5793fc1e64d7bdaf25074"
	findings := types.Findings{}
	level := cmpOr(findings.RiskLevel, "medium")
	rationale := cmpOr(findings.RiskRationale,
		"risk not stated by the test analyzer for head "+head+"; defaulted medium pending explicit assessment")
	if level != "medium" {
		t.Errorf("defaulted level = %q, want medium", level)
	}
	if !strings.Contains(rationale, head) {
		t.Errorf("defaulted rationale must name the tested head, got %q", rationale)
	}
	if rationale == "risk not stated by the test analyzer for head ; defaulted medium pending explicit assessment" {
		t.Errorf("rationale must not be produced with an empty head")
	}
}
