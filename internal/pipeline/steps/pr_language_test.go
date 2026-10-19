package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Exercise authoring -> assembly -> final digest-bound validation, not prompt
// tokens. The controlled writer translates; assembly must not reinsert source.
func TestPRWriterPublishesEnglishIntentWithoutChangingPrivateSource(t *testing.T) {
	for _, original := range []string{
		"Adicionar exportação CSV. Aprovação apenas para revisão; não publicar em produção.",
		"Add CSV export. Aprovação apenas para revisão; do not deploy.",
	} {
		t.Run(original, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			const translated = "Add CSV export. The approval scope is: \"For review only; do not deploy to production.\""
			private := "PRIVATE_REPAIR_SENTINEL_DO_NOT_PUBLISH"
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, private) {
					t.Fatal("private repair guidance reached public writer input")
				}
				payload, _ := json.Marshal(map[string]any{"title": "feat: add CSV export", "body": "## What Changed\n\n- Add CSV export for review only; no production deployment.", "public_intent": translated, "unsupported_rules": []string{}})
				return &agent.Result{Output: payload}, nil
			}}
			var validated int
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				raw := strings.SplitN(opts.Prompt, "Run-written PR content JSON:\n", 2)
				if len(raw) != 2 {
					t.Fatal("missing actual content")
				}
				var written struct{ Title, Body string }
				if err := json.Unmarshal([]byte(raw[1]), &written); err != nil {
					t.Fatal(err)
				}
				english := !strings.Contains(written.Body, "Aprovação")
				if strings.Contains(written.Body, private) {
					t.Fatal("private repair guidance leaked")
				}
				if english && !strings.Contains(written.Body, "## Intent\n\n"+translated) {
					t.Fatal("translated approval scope lost")
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: english, FormatCompliant: true, Issues: []string{}})
				validated++
				return &agent.Result{Output: payload}, nil
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.UserIntent = original
			sctx.IntentSource = db.RunIntentSourceAgent
			if err := sctx.DB.UpdateRunIntent(sctx.Run.ID, db.RunIntent{Summary: original, Source: db.RunIntentSourceAgent}); err != nil {
				t.Fatal(err)
			}
			// Private repair guidance is durable, but not public authoring input.
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, "review")
			if err != nil {
				t.Fatal(err)
			}
			findings := `{"summary":"review","items":[{"id":"scope","severity":"warning","description":"Review only; deployment remains unapproved.","action":"ask-user"}]}`
			round, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			selection := `[]`
			privateGuidance := `[{"id":"scope","user_instructions":"` + private + `"}]`
			if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUserDeclined, &privateGuidance); err != nil {
				t.Fatal(err)
			}
			before, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeRounds, err := sctx.DB.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(before)
			beforeRoundsJSON, _ := json.Marshal(beforeRounds)
			for _, limit := range []int{0, 4000} {
				content, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", base, scm.ProviderGitHub, limit)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(content.Body, original) || strings.Contains(content.Body, private) {
					t.Fatal("private source published")
				}
			}
			if validated != 2 || sctx.UserIntent != original {
				t.Fatal("validation skipped or in-memory source changed")
			}
			after, _ := sctx.DB.GetRun(sctx.Run.ID)
			afterRounds, _ := sctx.DB.GetRoundsByStep(sr.ID)
			afterJSON, _ := json.Marshal(after)
			afterRoundsJSON, _ := json.Marshal(afterRounds)
			if string(beforeJSON) != string(afterJSON) || string(beforeRoundsJSON) != string(afterRoundsJSON) {
				t.Fatal("publication changed persisted source or approval evidence")
			}
		})
	}
}

func TestPRWriterPublicProjectionStillRequiresEnglishAndScopeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, projection      string
		suppressed, wantError bool
	}{
		{name: "missing projection", wantError: true},
		{name: "untranslated quotation", projection: `The requested scope is "Aprovação apenas para revisão".`, wantError: true},
		{name: "invented approval", projection: "Approved for production deployment.", wantError: true},
		{name: "suppressed despite writer output", projection: "PRIVATE_PROJECTION_SENTINEL", suppressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, _ := json.Marshal(map[string]any{"title": "feat: add export", "body": "## What Changed\n\n- Add export for review.", "public_intent": tc.projection, "unsupported_rules": []string{}})
				return &agent.Result{Output: payload}, nil
			}}
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				raw := strings.SplitN(opts.Prompt, "Run-written PR content JSON:\n", 2)
				if len(raw) != 2 {
					t.Fatal("missing final content")
				}
				var content prContent
				if err := json.Unmarshal([]byte(raw[1]), &content); err != nil {
					t.Fatal(err)
				}
				if tc.suppressed && (strings.Contains(content.Body, "## Intent") || strings.Contains(content.Body, tc.projection)) {
					t.Fatal("suppressed intent reached public body")
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					t.Fatal(err)
				}
				verdict := prContentValidation{ContentSHA256: digest, English: !strings.Contains(content.Body, "Aprovação"), FormatCompliant: true, Issues: []string{}}
				if strings.Contains(content.Body, "Approved for production deployment") {
					verdict.Issues = append(verdict.Issues, "review approval was broadened to production")
				}
				payload, _ := json.Marshal(verdict)
				return &agent.Result{Output: payload}, nil
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.UserIntent = "Adicionar exportação CSV. Aprovação apenas para revisão."
			if tc.suppressed {
				no := false
				sctx.Config.PR.PublishIntent = &no
			}
			_, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestPRTemplatePublishesTranslatedQuotationWithoutAddingSourceIntent(t *testing.T) {
	sctx, ag, _ := templateTestContext(t)
	sctx.UserIntent = "Add export. O escopo aprovado é apenas revisão; produção não autorizada."
	original := sctx.UserIntent
	const quotation = "The approval scope is: \"Review only; production is not authorized.\""
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		body := strings.Replace(filledPRTemplate, "Add a Bar helper.", "Add export. "+quotation, 1)
		payload, _ := json.Marshal(templateDraft("feat: add export for review", body, testPRTemplate))
		return &agent.Result{Output: payload}, nil
	}
	ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		raw := strings.SplitN(opts.Prompt, "Run-written PR content JSON:\n", 2)
		if len(raw) != 2 {
			t.Fatal("missing final content")
		}
		var written prContent
		if err := json.Unmarshal([]byte(raw[1]), &written); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(written.Body, quotation) || strings.Contains(written.Body, original) {
			t.Fatal("template lost translated scope or copied private source")
		}
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{}})
		return &agent.Result{Output: payload}, nil
	}
	content, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sctx.UserIntent != original || strings.Contains(content.Body, "## Intent") {
		t.Fatal("template changed source or appended generated intent")
	}
}
