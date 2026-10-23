package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
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
		{"commit 0123456789abcdef0123456789abcdef01234567 and .no-mistakes.yaml", "commit 0123456789abcdef0123456789abcdef01234567 and .no-mistakes.yaml"},
	}
	for _, tc := range cases {
		if got := redactRunIdentifiers(tc.in); got != tc.want {
			t.Errorf("redactRunIdentifiers(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
	if !strings.Contains(ghLog, "pr create") || strings.Contains(ghLog, "gate-token-7f3k") || !strings.Contains(ghLog, "repair widget behind "+learnedPlaceholder) {
		t.Fatalf("retry did not publish the sanitized text:\n%s", ghLog)
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
	for _, want := range []string{"refusing non-compliant PR publication", "overstates", "no-mistakes rerun", "gh pr create --head feature --base main"} {
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
