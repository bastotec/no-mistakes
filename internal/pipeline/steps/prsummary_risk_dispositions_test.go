package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These are local publication-refusal fixtures, not observations of a product.
// The prose deliberately retains the old present-tense rationale after repair.
func TestPRRiskDispositionsOrdinaryAndOwnedPublication(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name, rationale string
		descriptions    []string
		unreviewed      bool
	}{
		{"display-freeze", "The display freezes while refreshing.", []string{"The display freezes while refreshing."}, false},
		{"ledger-error", "Ledger errors remain hidden.", []string{"Ledger errors remain hidden."}, true},
		{"equality-publication", "Equality is incorrect and publication can lose updates.", []string{"Equality is incorrect.", "Publication can lose updates."}, false},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			for _, unresolved := range []bool{false, true} {
				t.Run(map[bool]string{false: "repaired", true: "mixed-unresolved"}[unresolved], func(t *testing.T) {
					dir, base, head := setupGitRepo(t)
					ag := &mockAgent{name: "local-validator"}
					sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
					initial := types.Findings{RiskLevel: "high", RiskRationale: fixture.rationale}
					for i, description := range fixture.descriptions {
						initial.Items = append(initial.Items, types.Finding{ID: string(rune('a' + i)), Severity: "error", Description: description})
					}
					final := types.Findings{RiskLevel: "high", RiskRationale: fixture.rationale}
					if fixture.unreviewed {
						final.ResolvedByFix = initial.Items
						final.RoundCap = 1
					}
					if unresolved {
						final.Items = []types.Finding{{ID: "open", Severity: "error", Description: "An independent request still fails."}}
						final.UnresolvedNotes = []types.Finding{{ID: "note", Severity: "warning", Description: "An optional fallback remains unavailable."}}
					}
					initialJSON, _ := json.Marshal(initial)
					finalJSON, _ := json.Marshal(final)
					sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
					if err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.SetStepFindings(sr.ID, string(finalJSON)); err != nil {
						t.Fatal(err)
					}
					first, last, fix := string(initialJSON), string(finalJSON), "changes applied"
					if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &first, nil, 1); err != nil {
						t.Fatal(err)
					}
					if _, err := sctx.DB.InsertStepRound(sr.ID, 2, "auto_fix", &last, &fix, 1); err != nil {
						t.Fatal(err)
					}
					pipelineMD, risk, _ := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
					body := appendGeneratedSections("## What Changed\n\n- Repair validation behavior.", risk, "", pipelineMD)
					appendix, err := (&PRStep{}).buildPRAppendix(sctx, scm.ProviderGitHub)
					if err != nil {
						t.Fatal(err)
					}
					for _, text := range []string{body, appendix} {
						if !strings.Contains(text, "Historical review assessments (not current-behavior claims):") || !strings.Contains(text, "🚨 High: "+fixture.rationale) {
							t.Fatalf("lost historical high risk:\n%s", text)
						}
						if !strings.Contains(text, risk) {
							t.Fatalf("publication paths disagree:\n%s", text)
						}
						dispositions := strings.SplitN(risk, "Final recorded dispositions", 2)[1]
						for _, description := range fixture.descriptions {
							if strings.Contains(dispositions, description) && !fixture.unreviewed {
								t.Fatalf("repaired finding presented as current: %s", dispositions)
							}
						}
						if fixture.unreviewed && !strings.Contains(dispositions, "Fixed without rereview (residual risk unverified)") {
							t.Fatalf("invented rereview: %s", dispositions)
						}
						if unresolved && (!strings.Contains(dispositions, "An independent request still fails.") || !strings.Contains(dispositions, "An optional fallback remains unavailable.")) {
							t.Fatalf("lost unresolved evidence: %s", dispositions)
						}
						if !unresolved && !strings.Contains(dispositions, "No unresolved findings recorded.") {
							t.Fatalf("wrong disposition: %s", dispositions)
						}
						if strings.Count(text, pipelineAttestationCommentPrefix) != 1 || !strings.Contains(text, head) {
							t.Fatalf("lost exact-head attestation: %s", text)
						}
					}
					// Keep the final compliance boundary: a digest-bound agent rejection of
					// contradictory prose must refuse both ordinary and appendix publication.
					calls := 0
					ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
						calls++
						digest, err := validationDigestFromPrompt(opts.Prompt)
						if err != nil {
							return nil, err
						}
						output, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{"Repaired defects are described as current behavior."}})
						return &agent.Result{Output: output}, nil
					}
					for _, owned := range []bool{false, true} {
						candidate := prContent{Title: "fix: validation behavior", Body: body}
						if owned {
							candidate.Body = appendix
						}
						candidate.Body += "\n\nCurrent remaining defect: " + fixture.rationale
						err := (&PRStep{}).validateFinalPRContent(sctx, candidate, candidate, "", base, !owned)
						if err == nil || !strings.Contains(err.Error(), "refusing non-compliant PR publication") {
							t.Fatalf("contradictory content accepted: %v", err)
						}
					}
					if calls != 2 {
						t.Fatalf("final compliance check bypassed: %d calls", calls)
					}
				})
			}
		})
	}
}
