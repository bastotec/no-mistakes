package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestClassifyReview(t *testing.T) {
	defaults := config.ResolveReviewSizing(config.ReviewSizingRaw{})
	forced := config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "xhigh"})
	custom := config.ResolveReviewSizing(config.ReviewSizingRaw{RiskyPaths: &[]string{"internal/gate/**"}})
	noRisk := config.ResolveReviewSizing(config.ReviewSizingRaw{RiskyPaths: &[]string{}})
	w := func(files, lines int) *agent.InvocationWorkload {
		return &agent.InvocationWorkload{Files: files, Lines: lines}
	}
	for _, tc := range []struct {
		name     string
		sizing   config.ReviewSizing
		workload *agent.InvocationWorkload
		changed  []string
		want     string
		reason   string
	}{
		{"small", defaults, w(2, 40), []string{"a.go", "b.go"}, "low", "small change: 2 files, 40 changed lines"},
		{"normal", defaults, w(5, 300), []string{"a.go"}, "medium", "normal change"},
		{"large by lines", defaults, w(4, 800), []string{"a.go"}, "high", "large change"},
		{"large by files", defaults, w(20, 100), []string{"a.go"}, "high", "large change"},
		{"risky directory segment", defaults, w(1, 3), []string{"internal/Auth/session.go"}, "high", "touches risky path internal/Auth/session.go"},
		{"risky file name", defaults, w(1, 3), []string{"pkg/oauth.go"}, "high", "touches risky path pkg/oauth.go"},
		{"custom risky subtree", custom, w(1, 3), []string{"internal/gate/reconcile.go"}, "high", "risky path"},
		{"custom list replaces defaults", custom, w(1, 3), []string{"internal/auth.go"}, "low", "small change"},
		{"empty list turns path risk off", noRisk, w(1, 3), []string{"internal/auth.go"}, "low", "small change"},
		{"min effort raises a small change", forced, w(1, 3), []string{"a.go"}, "xhigh", "small change: 1 files, 3 changed lines; review.sizing.min_effort raises it to xhigh"},
		{"min effort never lowers a risky change", config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "medium"}), w(1, 3), []string{"internal/auth/x.go"}, "high", "touches risky path"},
		{"unknown size is never lighter", defaults, nil, []string{"a.go"}, "medium", "change size unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyReview(tc.sizing, tc.workload, tc.changed)
			if got != tc.want || !strings.Contains(reason, tc.reason) {
				t.Fatalf("classifyReview = %s (%s), want %s containing %q", got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestPlanReviewBoundsAgentsAndTurnsPerLevel(t *testing.T) {
	routes := config.ReviewRoutesRaw{Light: "light/route", Verify: "verify/route", Finders: []string{"a/route", "b/route"}}
	for _, tc := range []struct {
		effort    string
		finders   int
		verifyCap int
	}{
		{"low", 0, 0}, {"medium", 0, 0}, {"high", 4, 12}, {"xhigh", 5, 20}, {"max", 6, 30},
	} {
		sizing := config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: tc.effort})
		plan := planReview(sizing, routes, &agent.InvocationWorkload{}, nil)
		if len(plan.Dimensions) != tc.finders || plan.VerifyCap != tc.verifyCap {
			t.Fatalf("%s: %d finders, verify cap %d; want %d and %d", tc.effort, len(plan.Dimensions), plan.VerifyCap, tc.finders, tc.verifyCap)
		}
		if plan.multiAngle() && (plan.FinderTuning.MaxTurns == 0 || plan.VerifyTuning.MaxTurns == 0 || plan.VerifyTuning.Model != "verify/route") {
			t.Fatalf("%s: finder and verify agents must carry turn bounds and the verify route: %+v", tc.effort, plan)
		}
	}
	low := planReview(config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "low"}), routes, &agent.InvocationWorkload{}, nil)
	if low.Tuning == nil || low.Tuning.Model != "light/route" || low.Tuning.Effort != "low" || low.Tuning.MaxTurns == 0 {
		t.Fatalf("low review tuning = %+v, want the light route at low effort with a turn bound", low.Tuning)
	}
	if medium := planReview(config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "medium"}), routes, nil, nil); medium.Tuning != nil {
		t.Fatalf("medium review must keep today's reviewer untouched, got tuning %+v", medium.Tuning)
	}
	if finderRoute(routes, 0) != "a/route" || finderRoute(routes, 1) != "b/route" || finderRoute(routes, 2) != "a/route" {
		t.Fatal("finder routes must round-robin")
	}
}

// concurrentAgent is a goroutine-safe fake for the multi-angle review, which
// runs its agents in parallel.
type concurrentAgent struct {
	mu    sync.Mutex
	calls []agent.RunOpts
	run   func(opts agent.RunOpts) (*agent.Result, error)
}

func (a *concurrentAgent) Name() string { return "deck" }
func (a *concurrentAgent) Close() error { return nil }
func (a *concurrentAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.mu.Lock()
	a.calls = append(a.calls, opts)
	a.mu.Unlock()
	return a.run(opts)
}

func (a *concurrentAgent) callsMatching(substr string) []agent.RunOpts {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []agent.RunOpts
	for _, c := range a.calls {
		if strings.Contains(c.Prompt, substr) {
			out = append(out, c)
		}
	}
	return out
}

const verifyPromptMarker = "Verify one claimed code-review finding"

// multiAngleFake answers each finder by its dimension and each verifier by
// the claim it is given.
func multiAngleFake(t *testing.T) *concurrentAgent {
	t.Helper()
	finder := map[string]string{
		"correctness":    `{"findings":[{"severity":"error","action":"auto-fix","file":"a.txt","line":3,"description":"retry loop never stops when the queue is closed","review_scope":"source"},{"severity":"warning","action":"auto-fix","file":"a.txt","line":20,"description":"timeout value is parsed as seconds but documented as milliseconds","review_scope":"source"}],"risk_level":"medium","risk_rationale":"one loop defect","risk_scope":"source-or-external"}`,
		"security":       `{"findings":[],"risk_level":"low","risk_rationale":"no boundary touched","risk_scope":"source-or-external"}`,
		"regressions":    `{"findings":[{"severity":"warning","action":"auto-fix","file":"a.txt","line":3,"description":"Retry loop never stops when the queue is closed.","review_scope":"source"}],"risk_level":"medium","risk_rationale":"callers can hang","risk_scope":"source-or-external"}`,
		"simplification": `{"findings":[{"severity":"info","action":"no-op","file":"b.txt","line":1,"description":"helper duplicates strings.Cut","review_scope":"source"}],"risk_level":"high","risk_rationale":"outlier","risk_scope":"source-or-external"}`,
	}
	return &concurrentAgent{run: func(opts agent.RunOpts) (*agent.Result, error) {
		if strings.HasPrefix(opts.Prompt, verifyPromptMarker) {
			switch {
			case strings.Contains(opts.Prompt, "retry loop"):
				return &agent.Result{Output: json.RawMessage(`{"verdict":"confirmed","evidence":"a.txt:3 loops on a closed channel"}`)}, nil
			case strings.Contains(opts.Prompt, "timeout value"):
				return &agent.Result{Output: json.RawMessage(`{"verdict":"refuted","evidence":"a.txt:18 converts to milliseconds first"}`)}, nil
			default:
				return &agent.Result{Output: json.RawMessage(`{"verdict":"plausible","evidence":"could not run it"}`)}, nil
			}
		}
		for dim, out := range finder {
			if strings.Contains(opts.Prompt, "You are the "+dim+" finder") {
				return &agent.Result{Output: json.RawMessage(out)}, nil
			}
		}
		return nil, errors.New("unexpected prompt")
	}}
}

func TestReviewStep_MultiAngleReviewVerifiesDeduplicatesAndRanks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := multiAngleFake(t)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Sizing = config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "high"})
	sctx.Config.Review.Routes = config.ReviewRoutesRaw{Finders: []string{"chain/review", "claude/sonnet-5-5"}, Verify: "chain/review"}

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}

	finders := ag.callsMatching("Multi-angle review focus")
	if len(finders) != 4 {
		t.Fatalf("finder calls = %d, want one per high-level dimension", len(finders))
	}
	routes := map[string]int{}
	for _, c := range finders {
		if c.Tuning == nil || c.Tuning.MaxTurns == 0 || c.Session != nil || c.Purpose != "review" {
			t.Fatalf("finder call must be a session-free review with a turn bound: %+v", c)
		}
		routes[c.Tuning.Model]++
	}
	if routes["chain/review"] != 2 || routes["claude/sonnet-5-5"] != 2 {
		t.Fatalf("finder routes = %v, want the two routes spread across the four finders", routes)
	}
	// The two "retry loop" claims are one claim, so three verifiers run.
	if verifiers := ag.callsMatching(verifyPromptMarker); len(verifiers) != 3 {
		t.Fatalf("verify calls = %d, want one per de-duplicated claim (3)", len(verifiers))
	}

	if len(findings.Items) != 2 {
		t.Fatalf("findings = %+v, want the confirmed loop defect and the plausible nit (refuted claim dropped)", findings.Items)
	}
	first, second := findings.Items[0], findings.Items[1]
	if first.Severity != "error" || !strings.HasPrefix(first.Description, "[CONFIRMED] retry loop never stops when the queue is closed") || !strings.Contains(first.Description, "Verification: a.txt:3") {
		t.Fatalf("first finding = %+v, want the most severe copy marked CONFIRMED with its evidence", first)
	}
	if !strings.HasPrefix(second.Description, "[PLAUSIBLE] helper duplicates") {
		t.Fatalf("second finding = %+v, want the plausible nit", second)
	}
	for _, item := range findings.Items {
		if strings.Contains(item.Description, "timeout value") {
			t.Fatal("a refuted claim must not be reported")
		}
	}
	if findings.RiskLevel != "high" || !strings.Contains(findings.RiskRationale, "verified error") {
		t.Fatalf("risk = %s (%s), want high from the verified error", findings.RiskLevel, findings.RiskRationale)
	}
	if findings.ReviewEffort != "high" || !strings.HasSuffix(findings.ReviewEffortReason, "review.sizing.min_effort raises it to high") {
		t.Fatalf("review effort = %q (%q), want high recorded for the attestation", findings.ReviewEffort, findings.ReviewEffortReason)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a verified error finding must still park the review")
	}
}

// Only verified findings rate the change, so an alarmed finder with nothing
// that survives verification cannot make a clean change high risk.
func TestReviewStep_MultiAngleRiskComesOnlyFromVerifiedFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &concurrentAgent{run: func(opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, "You are the security finder") {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"high","risk_rationale":"worried","risk_scope":"source-or-external"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Sizing = config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "high"})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if findings.RiskLevel != "low" || len(findings.Items) != 0 || outcome.NeedsApproval {
		t.Fatalf("outcome = %+v risk %s, want a clean low-risk review", outcome, findings.RiskLevel)
	}
	if len(ag.callsMatching(verifyPromptMarker)) != 0 {
		t.Fatal("no claims means no verifiers")
	}
}

// A finder that cannot produce a review fails the step: an unrun angle must
// not read as a clean one.
func TestReviewStep_MultiAngleFinderFailureFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &concurrentAgent{run: func(opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, "You are the regressions finder") {
			return nil, errors.New("provider unavailable")
		}
		return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Sizing = config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "high"})
	sctx.Config.Review.Routes = config.ReviewRoutesRaw{Finders: []string{"r/one"}}

	if _, err := (&ReviewStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("err = %v, want the finder failure", err)
	}
	if n := len(ag.callsMatching("You are the regressions finder")); n != 1 {
		t.Fatalf("regressions finder calls = %d, want 1 (no extra retry layer)", n)
	}
}

// The verify cap bounds agents, not findings: a claim past it was never
// disproved, so it is kept as plausible.
func TestReviewStep_MultiAngleClaimsPastTheVerifyCapStayPlausible(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var items []string
	for i := 0; i < 13; i++ {
		items = append(items, fmt.Sprintf(`{"severity":"error","action":"auto-fix","file":"f%d.go","line":1,"description":"defect number %d","review_scope":"source"}`, i, i))
	}
	correctness := `{"findings":[` + strings.Join(items, ",") + `],"risk_level":"high","risk_rationale":"many","risk_scope":"source-or-external"}`
	ag := &concurrentAgent{run: func(opts agent.RunOpts) (*agent.Result, error) {
		if strings.HasPrefix(opts.Prompt, verifyPromptMarker) {
			return &agent.Result{Output: json.RawMessage(`{"verdict":"confirmed","evidence":"seen"}`)}, nil
		}
		if strings.Contains(opts.Prompt, "You are the correctness finder") {
			return &agent.Result{Output: json.RawMessage(correctness)}, nil
		}
		return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Sizing = config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "high"})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if n := len(ag.callsMatching(verifyPromptMarker)); n != 12 {
		t.Fatalf("verify calls = %d, want the high-level cap of 12", n)
	}
	plausible := 0
	for _, item := range findings.Items {
		if strings.HasPrefix(item.Description, "[PLAUSIBLE]") {
			plausible++
		}
	}
	if len(findings.Items) != 13 || plausible != 1 {
		t.Fatalf("findings = %d (%d plausible), want all 13 with the one past the cap plausible", len(findings.Items), plausible)
	}
	for _, item := range findings.Items {
		if strings.HasPrefix(item.Description, "[PLAUSIBLE]") && item.Action != "ask-user" {
			t.Fatalf("an unverified claim must park for a human, got action %q", item.Action)
		}
	}
	for _, c := range ag.calls {
		if !c.Parallel {
			t.Fatal("finder and verifier invocations must be marked parallel so repair replay never keys them by arrival order")
		}
	}
}

// A verifier that crashes did not disprove its claim, so the claim stays as
// PLAUSIBLE instead of being dropped.
func TestReviewStep_MultiAngleVerifierFailureKeepsTheClaimPlausible(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &concurrentAgent{run: func(opts agent.RunOpts) (*agent.Result, error) {
		if strings.HasPrefix(opts.Prompt, verifyPromptMarker) {
			return nil, errors.New("verifier crashed")
		}
		if strings.Contains(opts.Prompt, "You are the correctness finder") {
			return &agent.Result{Output: json.RawMessage(blockingReviewJSON)}, nil
		}
		return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Sizing = config.ResolveReviewSizing(config.ReviewSizingRaw{MinEffort: "high"})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 || !strings.HasPrefix(findings.Items[0].Description, "[PLAUSIBLE]") || !outcome.NeedsApproval {
		t.Fatalf("findings = %+v, want the unverified error kept as plausible and still parking", findings.Items)
	}
}

// A small change runs one light reviewer with the light route and records
// its level; the agent's own JSON cannot claim a different level.
func TestReviewStep_SmallChangeRunsOneLightReviewer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &concurrentAgent{run: func(agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","review_effort":"max"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.Routes = config.ReviewRoutesRaw{Light: "zai/glm-5.3"}

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 || ag.calls[0].Tuning == nil || ag.calls[0].Tuning.Model != "zai/glm-5.3" || ag.calls[0].Tuning.Effort != "low" {
		t.Fatalf("calls = %+v, want one light reviewer on the light route", ag.calls)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if findings.ReviewEffort != "low" || !strings.HasPrefix(findings.ReviewEffortReason, "small change") {
		t.Fatalf("review effort = %q (%q), want low", findings.ReviewEffort, findings.ReviewEffortReason)
	}
}

func TestDedupeFindingsKeepsDistinctClaimsApart(t *testing.T) {
	// Neighboring timeouts share most of their wording and are still two
	// defects.
	timeouts := dedupeFindings([]Findings{{Items: []Finding{
		{Severity: "warning", File: "srv.go", Line: 10, Description: "ReadTimeout multiplies by seconds instead of milliseconds"},
		{Severity: "warning", File: "srv.go", Line: 11, Description: "WriteTimeout multiplies by milliseconds instead of seconds"},
	}}}, []string{"correctness"})
	if len(timeouts) != 2 {
		t.Fatalf("timeout claims merged: %+v", timeouts)
	}
	readWrite := dedupeFindings([]Findings{{Items: []Finding{
		{Severity: "warning", File: "conn.go", Line: 7, Description: "The read timeout is not enforced after reconnect, allowing a stalled peer to keep the connection open indefinitely and exhaust the worker pool."},
		{Severity: "warning", File: "conn.go", Line: 8, Description: "The write timeout is not enforced after reconnect, allowing a stalled peer to keep the connection open indefinitely and exhaust the worker pool."},
	}}}, []string{"correctness"})
	if len(readWrite) != 2 {
		t.Fatalf("read and write timeout claims merged: %+v", readWrite)
	}
	sameLine := dedupeFindings([]Findings{{Items: []Finding{
		{Severity: "warning", File: "srv.go", Line: 10, Description: "ReadTimeout multiplies by seconds instead of milliseconds"},
		{Severity: "warning", File: "srv.go", Line: 10, Description: "WriteTimeout multiplies by milliseconds instead of seconds"},
	}}}, []string{"correctness"})
	if len(sameLine) != 2 {
		t.Fatalf("same-line claims about different identifiers merged: %+v", sameLine)
	}
	// A merged claim keeps the most cautious action any finder asked for.
	cautious := dedupeFindings([]Findings{
		{Items: []Finding{{Severity: "error", Action: "auto-fix", File: "a.go", Line: 5, Description: "retry budget is never reset after success"}}},
		{Items: []Finding{{Severity: "warning", Action: "ask-user", File: "a.go", Line: 5, Description: "Retry budget is never reset after success."}}},
	}, []string{"correctness", "regressions"})
	if len(cautious) != 1 || cautious[0].Severity != "error" || cautious[0].Action != "ask-user" {
		t.Fatalf("merged = %+v, want the error copy carrying ask-user", cautious)
	}
	results := []Findings{
		{Items: []Finding{{Severity: "warning", File: "a.go", Line: 10, Description: "missing nil check on config loader result"}}},
		{Items: []Finding{
			{Severity: "error", File: "a.go", Line: 12, Description: "Missing nil check on config loader result."},
			{Severity: "warning", File: "a.go", Line: 11, Description: "metric name typo in exporter label"},
			{Severity: "warning", File: "b.go", Line: 10, Description: "missing nil check on config loader result"},
		}},
	}
	got := dedupeFindings(results, []string{"correctness", "regressions"})
	if len(got) != 3 {
		t.Fatalf("candidates = %+v, want 3 (one merged, two distinct)", got)
	}
	if got[0].Severity != "error" || len(got[0].Dimensions) != 2 {
		t.Fatalf("merged claim = %+v, want the error copy found by both dimensions", got[0])
	}
}

// A deferred delivery claim is stripped after review, so it never rates the
// change.
func TestVerifiedRiskIgnoresDeferredDeliveryFindings(t *testing.T) {
	f := Findings{Items: []Finding{{Severity: "error", Action: "auto-fix", ReviewScope: "pipeline-owned-delivery", Description: "PR is not open yet"}}}
	verifiedRisk(&f)
	if f.RiskLevel != "low" {
		t.Fatalf("risk = %s (%s), want low", f.RiskLevel, f.RiskRationale)
	}
}
