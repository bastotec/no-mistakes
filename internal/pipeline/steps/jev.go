package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	jev "github.com/kunchenguid/no-mistakes/internal/jev"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FindingCategoryJev marks the advisory Jev signal's findings.
const FindingCategoryJev = "jev"

// jevQuestionLabels are the human words for the fixed advisory questions in
// findings and summaries. A question without an entry falls back to its name.
var jevQuestionLabels = map[string]string{
	"breaking":  "breaking change",
	"data_loss": "data loss",
	"security":  "security exposure",
	"bug":       "logic bug",
}

func jevQuestionLabel(name string) string {
	if label, ok := jevQuestionLabels[name]; ok {
		return label
	}
	return name
}

// JevStep is the pipeline's advisory evaluation signal. It submits the run's
// diff to the Jev evaluation model with a fixed set of typed boolean questions
// and reports the verdict and per-question probabilities as info findings and
// a summary line. It is ADVISORY ONLY and fail-safe by construction:
//
//   - It never gates: findings are severity info with action no-op, so the
//     executor neither parks for approval nor spends an auto-fix round, and the
//     outcome never needs approval. There is no code path from a Jev answer to
//     a blocked or failed run.
//   - Jev unavailable, unconfigured, misconfigured, or uncertain means
//     skip-and-report: the step completes as skipped with a reason naming the
//     cause, and the run proceeds untouched.
//   - The gateway key is resolved by reference at call time (environment
//     variable, then a parse-only secrets file) and is never logged, embedded
//     in findings, or included in error text.
//   - A Gemini model is refused outright regardless of configuration.
//
// The configuration is global-only (config.Jev): the signal spends the
// operator's gateway quota, so no repository - and no pushed branch - can
// enable, disable, or steer it. The question set is fixed in code
// (jev.AdvisoryQuestions) so a repository cannot steer what is asked either.
type JevStep struct {
	// endpointOverride points the gateway at another URL. It exists for
	// tests (an httptest server); production constructs JevStep{} and uses
	// the fixed gateway endpoint. It is deliberately not a config field:
	// no operator or repository has a reason to redirect the advisory
	// signal's traffic.
	endpointOverride string
}

// Name identifies the step in the run's pipeline sequence.
func (s *JevStep) Name() types.StepName { return types.StepJev }

// Execute evaluates the run's diff and returns the advisory outcome.
func (s *JevStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	// Same entry guard as every other post-review step: if the reviewed head
	// was rewritten out-of-band, refuse before any work (including spending
	// gateway quota on a diff that is not the reviewed change). This is the
	// shared integrity refusal, not a Jev verdict - it aborts the run exactly
	// like the review step's other successors, and is the only error path.
	if err := assertPipelineHeadContinuity(sctx, types.StepJev); err != nil {
		return nil, err
	}

	ctx := sctx.Ctx
	cfg := sctx.Config.Jev

	if !cfg.Enabled {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: "advisory Jev evaluation disabled (global config jev.enabled)"}, nil
	}
	if jev.IsGeminiModel(cfg.Model) {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: fmt.Sprintf("jev.model %q names a Gemini model; refusing to run Jev through Gemini", cfg.Model)}, nil
	}

	key, err := jev.ResolveKey(cfg.GatewayKeyEnv, cfg.SecretsFile)
	if err != nil {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: safeurl.RedactText(fmt.Sprintf("jev gateway key unreadable: %v", err))}, nil
	}
	if key == "" {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: fmt.Sprintf("jev gateway key not found (variable %s in the environment or %s)", cfg.GatewayKeyEnv, cfg.SecretsFile)}, nil
	}

	baseSHA, err := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, runIntegrationBranch(sctx))
	if err != nil {
		// An unreadable base would fail review; for an advisory signal the
		// honest report is a skip, never a run failure.
		return &pipeline.StepOutcome{Skipped: true, SkipReason: safeurl.RedactText(fmt.Sprintf("jev cannot resolve the diff base: %v", err))}, nil
	}

	state, truncated, diffErr := jevDiffState(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA, cfg.MaxDiffBytes)
	if diffErr != nil {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: safeurl.RedactText(fmt.Sprintf("jev cannot read the diff: %v", diffErr))}, nil
	}
	if strings.TrimSpace(state) == "" {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: "no changes to evaluate"}, nil
	}
	if truncated {
		sctx.Log(fmt.Sprintf("diff truncated to %d bytes for the advisory Jev evaluation", cfg.MaxDiffBytes))
	}

	callCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	client := jev.NewClient(key, jev.WithModel(cfg.Model), jev.WithEndpoint(s.endpointOverride))
	questions := jev.AdvisoryQuestions()
	result, err := client.Evaluate(callCtx, state, questions)
	if err != nil {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: safeurl.RedactText(fmt.Sprintf("jev unavailable: %v", err))}, nil
	}

	summary, findings := jevFindings(questions, result, cfg.Threshold)
	sctx.Log(fmt.Sprintf("Jev advisory verdict: %s", summary))
	if truncated {
		summary += " (diff truncated)"
		findings.Summary = summary
	}
	findingsRaw, err := json.Marshal(findings)
	if err != nil {
		return &pipeline.StepOutcome{Skipped: true, SkipReason: safeurl.RedactText(fmt.Sprintf("jev cannot encode findings: %v", err))}, nil
	}
	return &pipeline.StepOutcome{ExitCode: 0, Findings: string(findingsRaw)}, nil
}

// jevDiffState builds the evaluation state: the unified diff between base and
// head, bounded to maxBytes at a line boundary with a visible truncation
// marker so the bound is reported, never hidden.
func jevDiffState(ctx context.Context, workDir, baseSHA, headSHA string, maxBytes int) (state string, truncated bool, err error) {
	if maxBytes <= 0 {
		maxBytes = jev.MaxStateBytes
	}
	diff, err := git.Run(ctx, workDir, "diff", "--no-color", baseSHA+".."+headSHA)
	if err != nil {
		return "", false, err
	}
	if len(diff) <= maxBytes {
		return diff, false, nil
	}
	cut := diff[:maxBytes]
	if idx := strings.LastIndexByte(cut, '\n'); idx > 0 {
		cut = cut[:idx+1]
	}
	return fmt.Sprintf("%s\n[diff truncated: showing %d of %d bytes]\n", cut, len(cut), len(diff)), true, nil
}

// jevFindings turns the model's answers into the advisory findings payload.
// Only flagged and uncertain questions become finding items; clear answers
// stay in the summary so every probability remains visible without turning
// the fold into a wall of no-ops. Every item is severity info, action no-op:
// the signal never gates, parks, or fixes.
func jevFindings(questions []jev.Question, result *jev.Result, threshold float64) (summary string, findings Findings) {
	type answered struct {
		name string
		p    float64
		ok   bool
	}
	answers := make([]answered, 0, len(questions))
	var missing []string
	maxP := 0.0
	for _, q := range questions {
		p, ok := result.Probability(q.Name)
		answers = append(answers, answered{q.Name, p, ok})
		if !ok {
			missing = append(missing, q.Name)
			continue
		}
		if p > maxP {
			maxP = p
		}
	}

	verdict := jev.Verdict(result, questions, threshold)

	var flaggedWords, clearWords []string
	for _, a := range answers {
		if !a.ok {
			continue
		}
		switch {
		case a.p >= threshold:
			flaggedWords = append(flaggedWords, fmt.Sprintf("%s p=%.2f", jevQuestionLabel(a.name), a.p))
		case a.p > 1-threshold:
			flaggedWords = append(flaggedWords, fmt.Sprintf("%s uncertain p=%.2f", jevQuestionLabel(a.name), a.p))
		default:
			clearWords = append(clearWords, fmt.Sprintf("%s p=%.2f", jevQuestionLabel(a.name), a.p))
		}
	}

	var parts []string
	if len(flaggedWords) > 0 {
		parts = append(parts, strings.Join(flaggedWords, ", "))
	}
	if len(clearWords) > 0 {
		parts = append(parts, "clear: "+strings.Join(clearWords, ", "))
	}
	if len(missing) > 0 {
		labels := make([]string, 0, len(missing))
		for _, name := range missing {
			labels = append(labels, jevQuestionLabel(name))
		}
		parts = append(parts, "not answered: "+strings.Join(labels, ", "))
	}
	summary = fmt.Sprintf("Jev advisory signal: %s (%s)", verdict, strings.Join(parts, "; "))

	items := []Finding{}
	for _, q := range questions {
		p, ok := result.Probability(q.Name)
		if !ok {
			items = append(items, Finding{
				Severity:    "info",
				Action:      "no-op",
				Category:    FindingCategoryJev,
				Description: fmt.Sprintf("advisory: Jev did not answer the %s question; treating the signal as uncertain. Advisory only, never a gate.", jevQuestionLabel(q.Name)),
			})
			continue
		}
		if p <= 1-threshold {
			// A clear answer stays in the summary; only flagged and uncertain
			// questions become items worth a reader's attention.
			continue
		}
		word := "flags"
		if p < threshold {
			word = "is uncertain about"
		}
		items = append(items, Finding{
			Severity:    "info",
			Action:      "no-op",
			Category:    FindingCategoryJev,
			Description: fmt.Sprintf("advisory: Jev %s a %s (p=%.2f). %s Advisory signal only - never a gate; verify like any reviewer hint.", word, jevQuestionLabel(q.Name), p, q.Instructions),
		})
	}

	risk := "low"
	rationale := fmt.Sprintf("advisory evaluation signal: %s (max p=%.2f); informational only, the review step owns the verdict", verdict, maxP)
	if len(missing) == len(questions) {
		rationale = "advisory evaluation signal unanswered; informational only"
	}
	return summary, Findings{
		Items:         items,
		Summary:       summary,
		RiskLevel:     risk,
		RiskRationale: rationale,
		RiskScope:     "advisory signal on the whole diff",
	}
}
