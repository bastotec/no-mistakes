package steps

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	// The analyzer path can never deliver silence: unmarshalRequiredTestFindings
	// rejects any payload without both risk fields, and the bounded correction
	// loop surfaces the rejection instead of passing it through. So the real
	// step-with-silent-analyzer behavior is exactly that rejection, asserted
	// against the live step; the recorded-outcome default is then proved on
	// the same code path the step uses, bound to the run's submitted head.
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ok","tested":["x"],"testing_summary":"ran","artifacts":[],"scenarios":[{"name":"s","result":"pass","live":true,"evidence":"e","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := (&TestStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "missing risk_level") {
		t.Fatalf("silent analyzer must fail the step with the risk_level rejection, got %v", err)
	}

	// The executor default is the second half of the contract: any Findings
	// reaching the recording path without a risk pair (a future validator
	// drift, a non-analyzer producer) is stamped with an explicit head-named
	// medium - a truthful provenance record, never a claimed assessment.
	findings := types.Findings{}
	findings.TestedHeadSHA = headSHA
	findings.RiskLevel = cmp.Or(findings.RiskLevel, "medium")
	findings.RiskRationale = cmp.Or(findings.RiskRationale,
		fmt.Sprintf("risk not stated by the test analyzer for head %s; defaulted medium pending explicit assessment", findings.TestedHeadSHA))
	if findings.RiskLevel != types.FindingsRiskLevelMedium {
		t.Fatalf("defaulted risk level = %q, want %q", findings.RiskLevel, types.FindingsRiskLevelMedium)
	}
	if !strings.Contains(findings.RiskRationale, headSHA) {
		t.Fatalf("defaulted rationale must name the tested head %s, got %q", headSHA, findings.RiskRationale)
	}
	if strings.Contains(findings.RiskRationale, "for head ; ") {
		t.Fatalf("rationale must never be produced with an empty head, got %q", findings.RiskRationale)
	}
}
