package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRedactRunIdentifiers(t *testing.T) {
	t.Setenv("NM_HOME", "")
	const runID = "01M5MMEPDZ5K14XG3ENEJZ76R1"
	cases := []struct{ in, want string }{
		{"approve or abort (run " + runID + ").", "approve or abort (run [run id])."},
		{"anchored at refs/no-mistakes/sync/" + runID + " now", "anchored at [internal ref] now"},
		{"see ~/.no-mistakes/logs/" + runID + "/pr.log", "see ~/.no-mistakes/logs/[run id]/pr.log"},
		{"evidence https://github.com/o/r/blob/ev/" + runID + "/shot.png", "evidence https://github.com/o/r/blob/ev/" + runID + "/shot.png"},
		{"<!-- no-mistakes:pr-validation:" + runID + " -->", "<!-- no-mistakes:pr-validation:" + runID + " -->"},
		{"{\"url\":\"https://example.com\",\"run_id\":\"" + runID + "\"}", "{\"url\":\"https://example.com\",\"run_id\":\"[run id]\"}"},
		{"commit 0123456789abcdef0123456789abcdef01234567 and .no-mistakes.yaml", "commit 0123456789abcdef0123456789abcdef01234567 and .no-mistakes.yaml"},
	}
	for _, tc := range cases {
		if got := redactRunIdentifiers(tc.in); got != tc.want {
			t.Errorf("redactRunIdentifiers(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, delimiter := range []string{" ", "\t", "\n", "\u00a0", "\"", "'", "`", "<", ">", "(", ")", "[", "]", "{", "}", ","} {
		input := "https://example.com/" + runID + delimiter + runID + " refs/no-mistakes/sync/private"
		want := "https://example.com/" + runID + delimiter + runIDPlaceholder + " " + internalRefPlaceholder
		if got := redactRunIdentifiers(input); got != want {
			t.Errorf("delimiter %q: got %q, want %q", delimiter, got, want)
		}
	}
}

func TestPROwnershipPreservesAuthorIdentifiers(t *testing.T) {
	t.Parallel()
	const runID = "01M5MMEPDZ5K14XG3ENEJZ76R1"
	const privateRef = "refs/no-mistakes/sync/author-example"
	_, appendix := ownedFixture(t)
	appendix += "\nGenerated " + runID + " " + privateRef
	for _, managed := range []bool{false, true} {
		parts := prOwnedBody{
			before:  "Author reproduction /home/person/work " + runID + " " + privateRef + "\n\n",
			after:   "\nAuthor notes /home/person/notes " + runID + " " + privateRef,
			managed: managed,
		}
		content, err := composeOwnedPRContent(parts, "fix: sanitizes "+runID, appendix, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parsePROwnedBody(content.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantBefore := strings.ReplaceAll(parts.before, "/home/person", "~")
		if !managed {
			wantBefore += "\n\n"
		}
		if parsed.before != wantBefore || parsed.after != strings.ReplaceAll(parts.after, "/home/person", "~") {
			t.Fatalf("author spans changed: %+v", parsed)
		}
		if strings.Contains(content.Title, runID) || !strings.Contains(content.Body, "Generated [run id] [internal ref]") {
			t.Fatalf("run-written content was not sanitized: %+v", content)
		}
	}
}

func TestPRPublicationRepairPreservesURLsAndMachineMarkers(t *testing.T) {
	t.Parallel()
	const literal = "gate-token-7f3k"
	r := &prPublicationRepair{}
	r.learn([]string{"The body exposes private identifier `" + literal + "`."})
	const url = "https://github.com/o/r/blob/evidence/" + literal + ".png"
	const marker = "<!-- no-mistakes:pr-validation:" + literal + " -->"
	attestation := pipelineAttestationCommentPrefix + `{"note":"` + literal + `"}` + pipelineAttestationCommentClosingToken
	input := literal + " [evidence](" + url + ")\n" + marker + "\n" + attestation + "\n" + literal
	want := learnedPlaceholder + " [evidence](" + url + ")\n" + marker + "\n" + attestation + "\n" + learnedPlaceholder
	if got := r.scrub(input); got != want {
		t.Fatalf("scrub = %q, want %q", got, want)
	}
}

func TestPRPublicationRepairDoesNotGrowBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ literal, replacement string }{
		{"r42", "***"},
		{"gate-r42", "********"},
		{"gate-token-7f3k", learnedPlaceholder},
		{"私密值", "***"},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			r := &prPublicationRepair{}
			r.learn([]string{"The body exposes private identifier `" + tc.literal + "`."})
			prefix := strings.Repeat("x", 4000-scm.PRBodyLen(tc.literal))
			body := prefix + tc.literal
			got := r.scrub(body)
			if got != prefix+tc.replacement || len(got) > len(body) || scm.PRBodyLen(got) > 4000 {
				t.Fatalf("scrub grew or failed to redact the body: bytes=%d, units=%d", len(got), scm.PRBodyLen(got))
			}
		})
	}
}

func TestRedactPRContentNMHome(t *testing.T) {
	const runID = "01M5MMEPDZ5K14XG3ENEJZ76R1"
	for _, tc := range []struct{ root, want string }{
		{"/Users/alex/custom-nm", "[local path]/logs/[run id]/pr.log"},
		{"/srv/custom-nm", "[local path]/logs/[run id]/pr.log"},
		{"/Users/alex/.no-mistakes", "~/.no-mistakes/logs/[run id]/pr.log"},
		{`C:\Users\alex\.no-mistakes`, `~\.no-mistakes/logs/[run id]/pr.log`},
	} {
		t.Run(tc.root, func(t *testing.T) {
			t.Setenv("NM_HOME", tc.root)
			input := tc.root + "/logs/" + runID + "/pr.log"
			got := redactPRContent(prContent{Title: input, Body: input})
			if got.Title != tc.want || got.Body != tc.want {
				t.Fatalf("redactPRContent = %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestPRPublicationRepairLiteralClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		issue, literal string
		learn          bool
	}{
		{"The body exposes the private identifier `r42`.", "r42", true},
		{"The body exposes local path `/srv/private workspace/output.log`.", "/srv/private workspace/output.log", true},
		{"The body invents the test `TestTokenRefresh`, which is absent from the diff.", "TestTokenRefresh", false},
		{"The body claims `TestTokenRefresh` was added, but the diff has no such test.", "TestTokenRefresh", false},
		{"The body fabricates a credential test `TestRefresh`.", "TestRefresh", false},
		{"The private identifier `gate-r42` is absent from the diff.", "gate-r42", false},
		{"An unsupported claim about the internal token `gate-r42`.", "gate-r42", false},
		{"The claim that private identifier `gate-r42` was removed is unsupported by the diff.", "gate-r42", false},
		{"Private identifier `gate-r42` is unsupported by the diff.", "gate-r42", false},
		{"Private identifier `gate-r42` is not in the diff.", "gate-r42", false},
		{"Private identifier `gate-r42` does not exist.", "gate-r42", false},
		{"The body exposes private identifier `claim-token-r42`.", "claim-token-r42", true},
		{"The body exposes private identifier `ab`.", "ab", false},
		{"The body exposes private identifier ``.", "", false},
	} {
		t.Run(tc.issue, func(t *testing.T) {
			r := &prPublicationRepair{}
			added := r.learn([]string{tc.issue})
			if (added == 1) != tc.learn || len(r.issues) != 1 {
				t.Fatalf("learn = %d, issues = %q", added, r.issues)
			}
			want := tc.literal
			if tc.learn {
				want = learnedPlaceholder
				if tc.literal == "r42" {
					want = "***"
				}
			}
			if got := r.scrub(tc.literal); got != want {
				t.Fatalf("scrub = %q, want %q", got, want)
			}
		})
	}
}

func TestRedactPRContentPreservesLearnedHeadingRedaction(t *testing.T) {
	t.Setenv("NM_HOME", "")
	r := &prPublicationRepair{}
	r.learn([]string{"The heading exposes private identifier `gate-r42`."})
	content := prContent{
		Title: "Sanitizes headings",
		Body:  "## gate-r42",
		HeadingTranslations: []templateHeadingTranslation{{
			Source: "## gate-r42", English: "## gate-r42",
		}},
	}
	content = redactPRContent(r.scrubContent(content))
	heading := content.HeadingTranslations[0]
	if content.Body != "## ********" || heading.English != content.Body || !heading.PublicationRedacted {
		t.Fatalf("learned heading redaction was lost: %+v", content)
	}
	if err := validateFinalTemplateHeadings(content.Body, requiredTemplateHeadingTranslations("## gate-r42", content.HeadingTranslations)); err != nil {
		t.Fatal(err)
	}
}

func TestPRPublicationRepairLearnsOnlyPrivacyLiterals(t *testing.T) {
	t.Parallel()
	r := &prPublicationRepair{}
	r.learn([]string{
		"The Pipeline history exposes the internal run identifier `nm-run-abc123` in a finding.",
		"The body claims `TestWidgetRetries` was added, but the diff has no such test.",
	})
	if len(r.literals) != 1 || r.literals[0] != "nm-run-abc123" {
		t.Fatalf("literals = %q, want only the privacy literal", r.literals)
	}
	attestation := pipelineAttestationCommentPrefix + `{"note":"nm-run-abc123"}` + pipelineAttestationCommentClosingToken
	got := r.scrub("leaked nm-run-abc123\n" + attestation)
	if got != "leaked [redacted]\n"+attestation {
		t.Fatalf("scrub must strip prose and keep the attestation byte for byte, got %q", got)
	}
	if section := r.promptSection(); !strings.Contains(section, "TestWidgetRetries") || !strings.Contains(section, "nm-run-abc123") {
		t.Fatalf("re-draft feedback must carry every issue, got %q", section)
	}
}

// Regression for run 01M5MMEPDZ5K14XG3ENEJZ76R1: the Review history quoted a
// repair-budget finding carrying the run's own id, and the validator refused
// publication. The generated history must reach the validator and the forge
// with the id already gone.
func TestPRStep_RedactsRunIDFromGeneratedReviewHistory(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"fix: repair widget","body":"## What Changed\n\n- repair widget"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	runID := sctx.Run.ID
	ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		verdict := prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{}}
		if strings.Contains(opts.Prompt, runID) {
			verdict.Issues = []string{"The Pipeline section exposes the internal run identifier `" + runID + "`."}
		}
		payload, _ := json.Marshal(verdict)
		return &agent.Result{Output: payload}, nil
	}
	review, err := sctx.DB.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"repair-authority","severity":"warning","description":"Automatic repair authority exhausted for review; approve or abort after reviewing the remaining findings (run ` + runID + `)."}],"summary":"budget exhausted"}`
	if err := sctx.DB.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(review.ID, 1, "initial", &findings, nil, 500); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() = %v, want publication with the run id redacted", err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr create") {
		t.Fatalf("PR was not created:\n%s", ghLog)
	}
	if strings.Contains(ghLog, runID) {
		t.Fatalf("published PR text still carries the run id:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "(run "+runIDPlaceholder+")") {
		t.Fatalf("review history lost the redacted finding:\n%s", ghLog)
	}
}

func TestPRStep_RetriesAfterValidatorRefusesRunWrittenText(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	var draftPrompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		draftPrompts = append(draftPrompts, opts.Prompt)
		return &agent.Result{Output: json.RawMessage(`{"title":"fix: repair widget","body":"## What Changed\n\n- repair widget behind gate-token-7f3k"}`)}, nil
	}}
	validations := 0
	ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		validations++
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		verdict := prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{}}
		if validations == 1 {
			verdict.Issues = []string{"What Changed exposes the private identifier `gate-token-7f3k`."}
		}
		payload, _ := json.Marshal(verdict)
		return &agent.Result{Output: payload}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = "Remove gate-token-7f3k from prose.\n\n## Testing\n\nExercise privacy redaction."

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() = %v, want a sanitized retry to publish", err)
	}
	if validations != 2 {
		t.Fatalf("validator ran %d times, want refusal then pass", validations)
	}
	if len(draftPrompts) != 2 || !strings.Contains(draftPrompts[1], "Publication validator feedback") || !strings.Contains(draftPrompts[1], "gate-token-7f3k") {
		t.Fatalf("re-draft did not receive the validator's issues: %d drafts", len(draftPrompts))
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr create") || strings.Contains(ghLog, "gate-token-7f3k") || !strings.Contains(ghLog, "repair widget behind "+learnedPlaceholder) || !strings.Contains(ghLog, "Remove "+learnedPlaceholder+" from prose.") {
		t.Fatalf("retry did not publish the sanitized text:\n%s", ghLog)
	}
}

func TestPRStep_OwnedPublicationValidatesBeforeRetarget(t *testing.T) {
	t.Parallel()
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry succeeds", true: "retries exhausted"}[exhausted], func(t *testing.T) {
			sctx, ag, trusted := templateTestContext(t)
			gitCmd(t, sctx.WorkDir, "branch", "epic/feature", trusted)
			env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "main")
			bodyFile := filepath.Join(t.TempDir(), "body.md")
			const authorBody = "Human description to preserve."
			if err := os.WriteFile(bodyFile, []byte(authorBody), 0o644); err != nil {
				t.Fatal(err)
			}
			sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
			base, url := "epic/feature", "https://github.com/test/repo/pull/42"
			sctx.Run.PRBaseBranch = &base
			sctx.Run.PRURL = &url
			validations := 0
			ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				validations++
				logData, err := os.ReadFile(logFile)
				if err != nil {
					return nil, err
				}
				if strings.Contains(string(logData), "pr edit") || strings.Contains(string(logData), "pr create") {
					t.Fatalf("forge was mutated before validation:\n%s", logData)
				}
				digest, err := validationDigestFromPrompt(opts.Prompt)
				if err != nil {
					return nil, err
				}
				verdict := prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{}}
				if exhausted || validations == 1 {
					verdict.Issues = []string{"The appendix exposes a private identifier."}
				}
				payload, _ := json.Marshal(verdict)
				return &agent.Result{Output: payload}, nil
			}
			_, err := (&PRStep{}).Execute(sctx)
			if (err != nil) != exhausted {
				t.Fatalf("Execute = %v, exhausted = %v", err, exhausted)
			}
			logData, readErr := os.ReadFile(logFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			body, readErr := os.ReadFile(bodyFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if exhausted {
				if validations != maxPRPublicationRepairs+1 || strings.Contains(string(logData), "pr edit") || string(body) != authorBody {
					t.Fatalf("exhausted refusal changed the forge: validations=%d, body=%q, log=%s", validations, body, logData)
				}
			} else if validations != 2 || !strings.Contains(string(logData), "--base epic/feature") || !strings.Contains(string(body), authorBody) || !hasPRAppendixMarkers(string(body)) {
				t.Fatalf("validated retry did not retarget and publish: validations=%d, body=%q, log=%s", validations, body, logData)
			}
		})
	}
}

func TestPRStep_ExhaustedPublicationRepairNamesNextStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"fix: repair widget","body":"## What Changed\n\n- repair widget"}`)}, nil
	}}
	validations := 0
	ag.validationFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		validations++
		digest, err := validationDigestFromPrompt(opts.Prompt)
		if err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(prContentValidation{ContentSHA256: digest, English: true, FormatCompliant: true, Issues: []string{"The title overstates the change."}})
		return &agent.Result{Output: payload}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("Execute() succeeded, want an exhausted refusal")
	}
	for _, want := range []string{"refusing non-compliant PR publication", "overstates", "no pull request was opened or edited", "no-mistakes rerun", "gh pr create --head feature --base main"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
	if validations != maxPRPublicationRepairs+1 {
		t.Fatalf("validator ran %d times, want %d", validations, maxPRPublicationRepairs+1)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "pr create") {
		t.Fatalf("refused content reached publication:\n%s", logData)
	}
}
