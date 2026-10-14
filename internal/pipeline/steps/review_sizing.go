package steps

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// reviewDimension is one angle a multi-angle review's finders look from.
type reviewDimension struct {
	Name  string
	Focus string
}

var (
	dimensionCorrectness = reviewDimension{"correctness", "logic errors and computations that return a wrong value, label or set; edge cases; error handling; broken state transitions in the changed code"}
	dimensionSecurity    = reviewDimension{"security", "authorization, authentication and privacy boundaries; trust boundaries and injection; secret and credential handling; unsafe or fail-open defaults"}
	dimensionRegressions = reviewDimension{"regressions", "behavior the change breaks for existing callers: changed contracts and signatures, call sites and sibling paths left behind, compatibility with stored data and older peers, docs or tests that now state the wrong behavior"}
	dimensionSimplify    = reviewDimension{"simplification", "reuse of helpers the codebase already has, components the intent does not require, needless complexity, and wasted work or efficiency regressions on hot paths"}
	dimensionState       = reviewDimension{"state and concurrency", "durable state and persistence, crash and restart recovery, races, locking and ordering between concurrent actors"}
	dimensionContracts   = reviewDimension{"tests and contracts", "tests whose assertions would still pass with the code wrong, and mismatches between the change and the specs, schemas or documented contracts it claims to satisfy"}
)

// reviewPlan is how much review one change gets. Low and medium run one
// reviewer; high and above run one finder per dimension in parallel and
// verify each finding before it is reported. Every bound here is a hard cap:
// at most len(Dimensions) finders plus VerifyCap verifiers per review round,
// each with its own model-call bound.
type reviewPlan struct {
	Effort string
	Reason string
	// Tuning applies to the single reviewer of a low or medium review.
	Tuning *agent.InvocationTuning
	// Dimensions, FinderTuning, VerifyCap and VerifyTuning apply from high up.
	Dimensions   []reviewDimension
	FinderTuning agent.InvocationTuning
	VerifyCap    int
	VerifyTuning agent.InvocationTuning
}

func (p reviewPlan) multiAngle() bool { return len(p.Dimensions) > 0 }

// reviewVerifyConcurrency bounds how many verify agents run at once.
const reviewVerifyConcurrency = 4

// planReview sizes the review for a change whose diff stat is workload (files
// and added-plus-deleted lines) touching changed. Risky paths are matched
// against the complete changed set, like path instructions, so a pushed
// ignore_patterns entry cannot lighten the review of a risky file.
func planReview(sizing config.ReviewSizing, routes config.ReviewRoutesRaw, workload *agent.InvocationWorkload, changed []string) reviewPlan {
	effort, reason := classifyReview(sizing, workload, changed)
	plan := reviewPlan{Effort: effort, Reason: reason}
	switch effort {
	case config.ReviewEffortLow:
		plan.Tuning = &agent.InvocationTuning{Model: strings.TrimSpace(routes.Light), Effort: "low", MaxTurns: 80, DeadlineSecs: 900}
	case config.ReviewEffortMedium:
		// Today's single reviewer at the operator's configured model and
		// effort, with the level's own run bounds.
		plan.Tuning = &agent.InvocationTuning{MaxTurns: 400, DeadlineSecs: 1800}
	case config.ReviewEffortHigh:
		plan.Dimensions = []reviewDimension{dimensionCorrectness, dimensionSecurity, dimensionRegressions, dimensionSimplify}
		plan.FinderTuning = agent.InvocationTuning{Effort: "medium", MaxTurns: 150, DeadlineSecs: 1500}
		plan.VerifyCap = 12
		plan.VerifyTuning = agent.InvocationTuning{Effort: "medium", MaxTurns: 60, DeadlineSecs: 900}
	case config.ReviewEffortXHigh:
		plan.Dimensions = []reviewDimension{dimensionCorrectness, dimensionSecurity, dimensionRegressions, dimensionSimplify, dimensionState}
		plan.FinderTuning = agent.InvocationTuning{Effort: "high", MaxTurns: 200, DeadlineSecs: 1800}
		plan.VerifyCap = 20
		plan.VerifyTuning = agent.InvocationTuning{Effort: "high", MaxTurns: 80, DeadlineSecs: 1200}
	default: // max
		plan.Dimensions = []reviewDimension{dimensionCorrectness, dimensionSecurity, dimensionRegressions, dimensionSimplify, dimensionState, dimensionContracts}
		plan.FinderTuning = agent.InvocationTuning{Effort: "xhigh", MaxTurns: 300, DeadlineSecs: 1800}
		plan.VerifyCap = 30
		plan.VerifyTuning = agent.InvocationTuning{Effort: "high", MaxTurns: 100, DeadlineSecs: 1200}
	}
	plan.VerifyTuning.Model = strings.TrimSpace(routes.Verify)
	return plan
}

// finderRoute spreads the configured finder routes round-robin, so different
// models look at the change. Empty keeps the agent's configured model.
func finderRoute(routes config.ReviewRoutesRaw, i int) string {
	if len(routes.Finders) == 0 {
		return ""
	}
	return strings.TrimSpace(routes.Finders[i%len(routes.Finders)])
}

func classifyReview(sizing config.ReviewSizing, workload *agent.InvocationWorkload, changed []string) (string, string) {
	if sizing.Fixed != "" {
		return sizing.Fixed, "fixed at " + sizing.Fixed
	}
	effort, reason := sizeReview(sizing, workload, changed)
	if slices.Index(config.ReviewEffortLevels, sizing.MinEffort) > slices.Index(config.ReviewEffortLevels, effort) {
		return sizing.MinEffort, reason + "; review.sizing.min_effort raises it to " + sizing.MinEffort
	}
	return effort, reason
}

func sizeReview(sizing config.ReviewSizing, workload *agent.InvocationWorkload, changed []string) (string, string) {
	if risky := firstRiskyPath(changed, sizing.RiskyPaths); risky != "" {
		return config.ReviewEffortHigh, "touches risky path " + risky
	}
	if workload == nil {
		// An unreadable diff stat must not make the review lighter.
		return config.ReviewEffortMedium, "change size unknown"
	}
	files, lines := workload.Files, workload.Lines
	size := fmt.Sprintf("%d files, %d changed lines", files, lines)
	switch {
	case lines >= sizing.LargeMinLines || files >= sizing.LargeMinFiles:
		return config.ReviewEffortHigh, "large change: " + size
	case lines <= sizing.SmallMaxLines && files <= sizing.SmallMaxFiles:
		return config.ReviewEffortLow, "small change: " + size
	default:
		return config.ReviewEffortMedium, "normal change: " + size
	}
}

// firstRiskyPath returns the first changed path a risky glob matches. A
// pattern containing "/" matches like ignore_patterns; one without matches
// any single path segment, case-insensitively, so "*auth*" catches a
// directory as well as a file.
func firstRiskyPath(changed, patterns []string) string {
	for _, file := range changed {
		for _, pattern := range patterns {
			if strings.Contains(pattern, "/") {
				if matchIgnorePattern(file, pattern) {
					return file
				}
				continue
			}
			for _, segment := range strings.Split(file, "/") {
				if ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(segment)); ok {
					return file
				}
			}
		}
	}
	return ""
}
