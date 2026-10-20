package steps

import (
	"context"
	"encoding/json"
	"html"
	"os"
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
			const findingSource = "The criterion \"Aprovação apenas para revisão\" was broadened by `deploy --production` in `export.go`."
			const findingEnglish = "The criterion \"Approval for review only\" was broadened by `deploy --production` in `export.go`."
			private := "PRIVATE_REPAIR_SENTINEL_DO_NOT_PUBLISH"
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, private) {
					t.Fatal("private repair guidance reached public writer input")
				}
				payload, _ := json.Marshal(map[string]any{"title": "feat: add CSV export", "body": "## What Changed\n\n- Add CSV export for review only; no production deployment.", "public_intent": translated, "unsupported_rules": []string{}})
				return &agent.Result{Output: payload}, nil
			}}
			ag.translationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, private) {
					t.Fatal("private repair guidance reached publication translator")
				}
				raw := strings.SplitN(opts.Prompt, prProseInputPrefix, 2)
				var sources []string
				if len(raw) != 2 || json.Unmarshal([]byte(raw[1]), &sources) != nil {
					t.Fatal("missing publication prose")
				}
				found := false
				for i, source := range sources {
					if source == findingSource {
						sources[i] = findingEnglish
						found = true
					}
				}
				if !found {
					t.Fatal("review round quotation not supplied for translation")
				}
				payload, _ := json.Marshal(map[string]any{"translations": sources})
				return &agent.Result{Output: payload}, nil
			}
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
			const cleanFindings = `{"summary":"review complete","items":[],"risk_level":"low","risk_rationale":"Review only.","review_effort":"medium","review_effort_reason":"small change"}`
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, "review")
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(sr.ID, cleanFindings); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.CompleteStep(sr.ID, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
			findingPayload, _ := json.Marshal(map[string]any{"summary": "review", "items": []any{map[string]any{"id": "scope", "severity": "warning", "description": findingSource, "action": "ask-user"}}})
			findings := string(findingPayload)
			round, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			selection := `["scope"]`
			privateGuidance := `[{"id":"scope","user_instructions":"` + private + `"}]`
			if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, &privateGuidance); err != nil {
				t.Fatal(err)
			}
			clean, fixed := cleanFindings, changesAppliedSummary
			if _, err := sctx.DB.InsertStepRound(sr.ID, 2, "auto_fix", &clean, &fixed, 0); err != nil {
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
			beforeSteps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			attestation := buildPipelineAttestation(beforeSteps, map[string][]*db.StepRound{sr.ID: beforeRounds}, head)
			beforeJSON, _ := json.Marshal(before)
			beforeStepsJSON, _ := json.Marshal(beforeSteps)
			beforeRoundsJSON, _ := json.Marshal(beforeRounds)
			for _, limit := range []int{0, 4000} {
				content, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", base, scm.ProviderGitHub, limit)
				if err != nil {
					t.Fatal(err)
				}
				publicBody := html.UnescapeString(content.Body)
				if strings.Contains(publicBody, original) || strings.Contains(publicBody, private) || strings.Contains(publicBody, findingSource) {
					t.Fatal("private source published")
				}
				if !strings.Contains(publicBody, findingEnglish) || !strings.Contains(publicBody, "Fixed in round 2") || !strings.Contains(content.Body, attestation) {
					t.Fatal("publication lost translated history, fix status, or unchanged attestation")
				}
			}
			if validated != 2 || sctx.UserIntent != original {
				t.Fatal("validation skipped or in-memory source changed")
			}
			after, _ := sctx.DB.GetRun(sctx.Run.ID)
			afterRounds, _ := sctx.DB.GetRoundsByStep(sr.ID)
			afterSteps, _ := sctx.DB.GetStepsByRun(sctx.Run.ID)
			afterJSON, _ := json.Marshal(after)
			afterStepsJSON, _ := json.Marshal(afterSteps)
			afterRoundsJSON, _ := json.Marshal(afterRounds)
			if string(beforeJSON) != string(afterJSON) || string(beforeRoundsJSON) != string(afterRoundsJSON) || string(beforeStepsJSON) != string(afterStepsJSON) {
				t.Fatal("publication changed persisted source or approval evidence")
			}
		})
	}
}

func TestPRWriterTranslatesTestedProseWithoutChangingCommandsOrRecords(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	const command = "`./check-scope --fixture 'fixtures/aprovação.json'`"
	const stepSource = `Verified the criterion "Aprovação apenas para revisão" against the running product`
	const stepEnglish = `Verified the criterion "Approval for review only" against the running product`
	const roundSource = `Checked "Não publicar em produção" using ` + command
	const roundEnglish = `Checked "Do not deploy to production" using ` + command
	ag := &mockAgent{name: "test", translationFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		raw := strings.SplitN(opts.Prompt, prProseInputPrefix, 2)
		var sources []string
		if len(raw) != 2 || json.Unmarshal([]byte(raw[1]), &sources) != nil {
			t.Fatal("missing publication prose")
		}
		translations := map[string]string{stepSource: stepEnglish, roundSource: roundEnglish, command: command}
		seen := map[string]bool{}
		for i, source := range sources {
			if translated, ok := translations[source]; ok {
				sources[i] = translated
				seen[source] = true
			}
		}
		if len(seen) != len(translations) {
			t.Fatalf("tested entries omitted from translation: %v", seen)
		}
		payload, _ := json.Marshal(map[string]any{"translations": sources})
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	stepPayload, _ := json.Marshal(map[string]any{"items": []any{}, "tested": []string{stepSource, command}})
	sr := insertCompletedStep(t, sctx, "test", string(stepPayload), "")
	roundPayload, _ := json.Marshal(map[string]any{"items": []any{}, "tested": []string{roundSource, command}})
	roundFindings := string(roundPayload)
	if _, err := sctx.DB.InsertStepRound(sr.ID, 2, "auto_fix", &roundFindings, nil, 0); err != nil {
		t.Fatal(err)
	}
	beforeSteps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRounds, err := sctx.DB.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeStepsJSON, _ := json.Marshal(beforeSteps)
	beforeRoundsJSON, _ := json.Marshal(beforeRounds)
	attestation := buildPipelineAttestation(beforeSteps, map[string][]*db.StepRound{sr.ID: beforeRounds}, head)
	pipelineMD, _, testingMD, err := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	public := html.UnescapeString(pipelineMD + testingMD)
	if strings.Contains(public, stepSource) || strings.Contains(public, roundSource) {
		t.Fatalf("publication retained untranslated tested prose: %s", public)
	}
	for _, want := range []string{stepEnglish, roundEnglish, command, attestation} {
		if !strings.Contains(html.UnescapeString(pipelineMD), want) {
			t.Fatalf("pipeline lost translated observation, literal command, or machine attestation %q: %s", want, pipelineMD)
		}
	}
	afterSteps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRounds, err := sctx.DB.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterStepsJSON, _ := json.Marshal(afterSteps)
	afterRoundsJSON, _ := json.Marshal(afterRounds)
	if string(beforeStepsJSON) != string(afterStepsJSON) || string(beforeRoundsJSON) != string(afterRoundsJSON) {
		t.Fatal("publication changed persisted Test evidence")
	}
}

func TestPRWriterTranslatesArtifactCaptionsWithoutChangingLiteralEvidence(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			const caption = `Verified the criterion "Aprovação apenas para revisão" against the running product`
			const english = `Verified the criterion "Approval for review only" against the running product`
			const fileBody = "RAW_FILE: aprovação apenas para revisão\nexit=0\n"
			const logBody = `{"criterion":"Aprovação apenas para revisão","exit":0}`
			const commandBody = "./check --scope 'revisão'\nresult=aprovado"
			evidenceRoot := t.TempDir()
			path := writeTempEvidenceFile(t, evidenceRoot, "aprovação.log", []byte(fileBody))
			ag := &mockAgent{name: "test", translationFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				raw := strings.SplitN(opts.Prompt, prProseInputPrefix, 2)
				var sources []string
				if len(raw) != 2 || json.Unmarshal([]byte(raw[1]), &sources) != nil {
					t.Fatal("missing publication prose")
				}
				found := false
				for i, source := range sources {
					if strings.Contains(source, "RAW_FILE:") || source == logBody || source == commandBody {
						t.Fatal("literal evidence reached translation")
					}
					if source == caption {
						sources[i] = english
						found = true
					}
				}
				if !found {
					t.Fatal("artifact caption omitted from translation")
				}
				payload, _ := json.Marshal(map[string]any{"translations": sources})
				return &agent.Result{Output: payload}, nil
			}}
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				raw := strings.SplitN(opts.Prompt, "Run-written PR content JSON:\n", 2)
				var written prContent
				if len(raw) != 2 || json.Unmarshal([]byte(raw[1]), &written) != nil {
					t.Fatal("missing final content")
				}
				// This checks the generated validator request contract, not the
				// model's interpretation of the literal evidence below.
				if !strings.Contains(opts.Prompt, "captured logs and command output") || !strings.Contains(opts.Prompt, "Judge authored captions and observations") {
					t.Fatal("validator request lost the literal-output versus authored-prose distinction")
				}
				body := html.UnescapeString(written.Body)
				if strings.Count(body, caption) != 2 || strings.Count(body, english) != 3 {
					t.Fatalf("final body mixed authored captions with literal output: %s", body)
				}
				for _, literal := range []string{strings.TrimRight(fileBody, "\n"), logBody, commandBody} {
					if !strings.Contains(body, literal) {
						t.Fatalf("final body changed literal evidence %q: %s", literal, body)
					}
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{}})
				return &agent.Result{Output: payload}, nil
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.EvidenceDir = evidenceRoot
			payload, _ := json.Marshal(map[string]any{
				"items": []any{}, "testing_summary": "Verified review-only scope.",
				"artifacts": []any{
					map[string]string{"kind": "log", "label": "Captioned log", "path": path, "content": caption},
					map[string]string{"kind": "screenshot", "label": "Captioned screenshot", "url": "https://example.com/evidence.png", "content": caption},
					map[string]string{"kind": "other", "label": "Inline observation", "content": caption},
					map[string]string{"kind": "log", "label": "Raw log", "content": logBody},
					map[string]string{"kind": "command-output", "label": "Raw command output", "content": commandBody},
					map[string]string{"kind": "log", "label": "Captured criterion log", "content": caption},
					map[string]string{"kind": "command-output", "label": "Captured criterion command output", "content": caption},
				},
			})
			sr := insertCompletedStep(t, sctx, "test", string(payload), "")
			roundFindings := string(payload)
			if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &roundFindings, nil, 0); err != nil {
				t.Fatal(err)
			}
			beforeSteps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeRounds, err := sctx.DB.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeStepsJSON, _ := json.Marshal(beforeSteps)
			beforeRoundsJSON, _ := json.Marshal(beforeRounds)
			attestation := buildPipelineAttestation(beforeSteps, map[string][]*db.StepRound{sr.ID: beforeRounds}, head)
			content, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", base, provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(html.UnescapeString(content.Body), english) != 3 {
				t.Fatalf("not all artifact captions translated: %s", content.Body)
			}
			if provider == scm.ProviderGitHub && !strings.Contains(content.Body, attestation) {
				t.Fatal("publication changed machine attestation")
			}
			if err := translatePRRenderProse(sctx, beforeSteps, map[string][]*db.StepRound{sr.ID: beforeRounds}); err != nil {
				t.Fatal(err)
			}
			full := html.UnescapeString(BuildTestingSummaryForPR(beforeSteps, map[string][]*db.StepRound{sr.ID: beforeRounds}, sctx.Repo.UpstreamURL, head, dir, evidenceRoot, nil))
			if strings.Count(full, caption) != 2 || strings.Count(full, english) != 3 {
				t.Fatalf("noncompact renderer mixed authored captions with literal output: %s", full)
			}
			for _, literal := range []string{strings.TrimRight(fileBody, "\n"), logBody, commandBody} {
				if !strings.Contains(full, literal) {
					t.Fatalf("noncompact renderer changed literal evidence %q: %s", literal, full)
				}
			}
			afterSteps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterRounds, err := sctx.DB.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterStepsJSON, _ := json.Marshal(afterSteps)
			afterRoundsJSON, _ := json.Marshal(afterRounds)
			if string(beforeStepsJSON) != string(afterStepsJSON) || string(beforeRoundsJSON) != string(afterRoundsJSON) {
				t.Fatal("publication changed durable artifact records")
			}
			unchangedFile, err := os.ReadFile(path)
			if err != nil || string(unchangedFile) != fileBody {
				t.Fatalf("publication changed literal file bytes: %q, %v", unchangedFile, err)
			}
		})
	}
}

func TestPRWriterPublicProjectionStillRequiresEnglishAndScopeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, projection, narrative string
		suppressed, wantError       bool
	}{
		{name: "missing projection", wantError: true},
		{name: "untranslated quotation", projection: `The requested scope is "Aprovação apenas para revisão".`, wantError: true},
		{name: "invented approval", projection: "Approved for production deployment.", wantError: true},
		{name: "suppressed despite writer output", projection: "PRIVATE_PROJECTION_SENTINEL", suppressed: true},
		{name: "suppressed intent still constrains narrative scope", projection: "PRIVATE_PROJECTION_SENTINEL", narrative: "Approved for production deployment.", suppressed: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				narrative := tc.narrative
				if narrative == "" {
					narrative = "Add export for review."
				}
				payload, _ := json.Marshal(map[string]any{"title": "feat: add export", "body": "## What Changed\n\n- " + narrative, "public_intent": tc.projection, "unsupported_rules": []string{}})
				return &agent.Result{Output: payload}, nil
			}}
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				reference := strings.SplitN(opts.Prompt, "Source intent JSON string (reference only):\n", 2)
				var source string
				if len(reference) != 2 || json.Unmarshal([]byte(strings.SplitN(reference[1], "\n\n", 2)[0]), &source) != nil || source != "Adicionar exportação CSV. Aprovação apenas para revisão." {
					t.Fatal("validator lost private approval-scope reference")
				}
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

func TestPRWriterRejectsIncompleteRoundTranslation(t *testing.T) {
	for _, payload := range []string{`{}`, `{"translations":[]}`, `{"translations":[""]}`, `{"translations":["Review only.","extra"]}`, `not JSON`} {
		t.Run(payload, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", translationFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: []byte(payload)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			const findings = `{"items":[{"id":"scope","severity":"warning","description":"Aprovação apenas para revisão","action":"ask-user"}]}`
			sr := insertCompletedStep(t, sctx, "review", findings, "")
			_, err := (&PRStep{}).buildPRContentForTest(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
			if err == nil || !strings.Contains(err.Error(), "PR publication prose") {
				t.Fatalf("incomplete translation was not refused: %v", err)
			}
			stored, err := sctx.DB.GetStepResult(sr.ID)
			if err != nil || stored.FindingsJSON == nil || *stored.FindingsJSON != findings {
				t.Fatalf("translation changed recorded findings: %v", err)
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
