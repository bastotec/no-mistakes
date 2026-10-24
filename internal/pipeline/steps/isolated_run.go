package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runconfine"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The whole-run confinement worker gets its run's state and the confinable
// steps' config - never global credentials, forge profiles, agent command
// overrides, the daemon database, or sockets. See runconfine.Policy.
type isolatedRunInput struct {
	Run                                                                                                                 *db.Run
	Repo                                                                                                                *db.Repo
	Config                                                                                                              config.Config
	Model                                                                                                               string
	Policy                                                                                                              runconfine.Policy
	PolicyDigest                                                                                                        string
	WorkDir, GateDir, EvidenceDir, StepResultID, StepName, PreviousFindings, DeferredFindings, UserIntent, IntentSource string
	Fixing                                                                                                              bool
	FixWithoutRereview                                                                                                  bool
	ReviewStartingHeadSHA                                                                                               string
	UncertifiedFromSHA, UncertifiedToSHA, UncertifiedSourceRunID                                                        string
	UncertifiedPriorRounds                                                                                              []*db.StepRound
	Round                                                                                                               int
	Gate                                                                                                                *config.Gate
	PriorBranchDecisions                                                                                                []*db.BranchDecisionRound
	PriorBranchDecisionsTruncated                                                                                       bool
}

// executeIsolatedRunStep runs one confinable step's whole execution inside the
// shared confinement boundary. One namespace per invocation, exactly like the
// Test-only worker: fresh storage, fresh database cluster, fresh model channel
// - "one dedicated isolation worker" is the shared worker machinery and the
// single policy binding every invocation, not a namespace that persists state
// between steps (that would be a handoff channel the design forbids).
func executeIsolatedRunStep(sctx *pipeline.StepContext, step pipeline.Step) (*pipeline.StepOutcome, error) {
	return sctx.RunIsolatedTestRepair(func(markStarted func(int) error) (*pipeline.StepOutcome, error) {
		return executeIsolatedRunInvocation(sctx, step, markStarted)
	})
}

func init() {
	// The executor dispatches a confined run's confinable step through this
	// proxy instead of the native step. pipeline cannot import steps (steps
	// imports pipeline), so the hook registered here is the seam.
	pipeline.SetConfinedStepInvoker(func(step pipeline.Step) pipeline.Step { return confinedRunStep{step: step} })
}

// confinedRunStep is the executor-side proxy for one confinable step of a
// confined run. It carries the wrapped step only for its identity and its
// custom-gate definition; execution happens entirely in the worker.
type confinedRunStep struct {
	step pipeline.Step
}

func (s confinedRunStep) Name() types.StepName { return s.step.Name() }

func (s confinedRunStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return executeIsolatedRunStep(sctx, s.step)
}

func executeIsolatedRunInvocation(sctx *pipeline.StepContext, step pipeline.Step, markStarted func(int) error) (outcome *pipeline.StepOutcome, invocationErr error) {
	defer func() {
		if cause := context.Cause(sctx.Ctx); cause != nil {
			outcome = nil
			invocationErr = errors.Join(invocationErr, cause)
		}
	}()
	if sctx.Agent == nil || sctx.Agent.Name() != "deck" {
		return nil, fmt.Errorf("run-confinement-adapter: Deck is required")
	}
	if err := testisolation.Check(); err != nil {
		return nil, fmt.Errorf("run-confinement-substrate: %w", err)
	}
	policy, digest, err := runconfinePolicyForDispatch(sctx)
	if err != nil {
		return nil, err
	}
	if sctx.EvidenceDir == "" {
		return nil, fmt.Errorf("run-confinement-storage: evidence directory is required")
	}
	if err := os.MkdirAll(sctx.EvidenceDir, 0700); err != nil {
		return nil, err
	}
	root := filepath.Join(filepath.Dir(sctx.EvidenceDir), "."+sctx.Run.ID+"-run-confinement")
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, fmt.Errorf("run-confinement-storage: create worker state (preserve any prior state for reconciliation): %w", err)
	}
	keepWorkspace := false
	defer func() {
		if !keepWorkspace {
			os.RemoveAll(root)
		}
	}()
	state := filepath.Join(root, "state.db")
	if err := sctx.DB.ExportTestSnapshot(state, sctx.Run.ID); err != nil {
		return nil, err
	}
	worker, err := os.Executable()
	if err != nil {
		return nil, err
	}
	deck, err := isolatedDeckPath(sctx.Config)
	if err != nil {
		return nil, err
	}
	workspace, err := testisolation.PrepareGit(sctx.Ctx, sctx.WorkDir, root, sctx.Run.BaseSHA, integrationMergeBaseRefs(sctx, runIntegrationBranch(sctx))...)
	if err != nil {
		return nil, err
	}
	postgresRoot, err := testisolation.PostgresRoot()
	if err != nil {
		return nil, fmt.Errorf("run-confinement-substrate: %w", err)
	}
	model := testisolation.ModelConfigFromEnvironment()
	invocation, err := testisolation.Start(sctx.Ctx, testisolation.Options{
		Worker: worker, Deck: deck, State: state, WorkDir: sctx.WorkDir, Git: workspace, EvidenceDir: sctx.EvidenceDir, ScratchParent: root, PostgresRoot: postgresRoot, Model: model, Entry: testisolation.EntryRun,
	})
	if err != nil {
		return nil, fmt.Errorf("run-confinement-confinement: %w", err)
	}
	defer invocation.Close()
	input := newIsolatedRunInput(sctx, step, model.Model, policy, digest)
	outcome, keepWorkspace, err = executeIsolatedWorker(sctx, invocation.Command, input, step.Name(), markStarted)
	return outcome, err
}

func newIsolatedRunInput(sctx *pipeline.StepContext, step pipeline.Step, model string, policy runconfine.Policy, digest string) isolatedRunInput {
	var gate *config.Gate
	if custom, ok := step.(*CustomGateStep); ok {
		g := custom.Gate
		gate = &g
	}
	var priorRounds []*db.StepRound
	for _, r := range sctx.UncertifiedPriorRounds {
		if r != nil {
			priorRounds = append(priorRounds, isolatedStepRound(r))
		}
	}
	return isolatedRunInput{Run: sctx.Run, Repo: confinedRepo(sctx.Repo), Config: confinedRunConfig(sctx.Config), Model: model, Policy: policy, PolicyDigest: digest,
		WorkDir: sctx.WorkDir, GateDir: "/nm/git", EvidenceDir: sctx.EvidenceDir, StepResultID: sctx.StepResultID, StepName: string(step.Name()),
		PreviousFindings: sctx.PreviousFindings, DeferredFindings: sctx.DeferredFindings, UserIntent: sctx.UserIntent, IntentSource: sctx.IntentSource,
		Fixing: sctx.Fixing, FixWithoutRereview: sctx.FixWithoutRereview, ReviewStartingHeadSHA: sctx.ReviewStartingHeadSHA,
		UncertifiedFromSHA: sctx.UncertifiedFromSHA, UncertifiedToSHA: sctx.UncertifiedToSHA, UncertifiedSourceRunID: sctx.UncertifiedSourceRunID, UncertifiedPriorRounds: priorRounds,
		Round: isolatedRound(sctx), Gate: gate,
		PriorBranchDecisions: isolatedBranchDecisions(sctx.PriorBranchDecisions), PriorBranchDecisionsTruncated: sctx.PriorBranchDecisionsTruncated}
}

// runconfinePolicyForDispatch re-derives the policy digest before every
// dispatch and refuses on any mismatch with the durable pin: a receipt minted
// under one policy must never be replayed under another.
func runconfinePolicyForDispatch(sctx *pipeline.StepContext) (runconfine.Policy, string, error) {
	policy := runconfine.DefaultPolicy()
	if err := policy.Validate(); err != nil {
		return runconfine.Policy{}, "", err
	}
	if sctx.Run.SubmittedHeadSHA == nil || *sctx.Run.SubmittedHeadSHA == "" {
		return runconfine.Policy{}, "", fmt.Errorf("run-confinement-policy: missing submitted head")
	}
	digest := runconfine.PolicyDigest(policy, sctx.Run.ID, *sctx.Run.SubmittedHeadSHA)
	pinned, ok, err := sctx.DB.RunConfinementDigest(sctx.Run.ID)
	if err != nil {
		return runconfine.Policy{}, "", fmt.Errorf("run-confinement-policy: read pinned digest: %w", err)
	}
	if !ok {
		return runconfine.Policy{}, "", fmt.Errorf("run-confinement-policy: run is not confined; refusing to dispatch a confined invocation")
	}
	if pinned != digest {
		return runconfine.Policy{}, "", fmt.Errorf("run-confinement-policy: pinned digest %s does not match the policy digest for this submitted head; refusing to reinterpret the confinement pin", pinned)
	}
	return policy, digest, nil
}

// RunIsolatedRunWorker is the whole-run confinement worker's internal binary
// entrypoint, reached only through the bubblewrap namespace the daemon built.
// No ordinary CLI launch calls it, and no ambient marker disables isolation.
func RunIsolatedRunWorker(input io.Reader, output io.Writer) error {
	if err := testisolation.RestrictWorkerNetwork(); err != nil {
		return err
	}
	decoder := json.NewDecoder(input)
	var request isolatedRunInput
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	return runIsolatedRunRequest(request, decoder, output)
}

// validateIsolatedRunRequest owns the request-shape refusals the worker makes
// before it touches any namespace-fixed path: the policy must be one this
// binary honors and the step must be inside the policy's scope. It is split
// out of the entrypoint so the contract is testable without first performing
// the network self-restriction, which is not undoable inside one process.
func validateIsolatedRunRequest(request isolatedRunInput) error {
	if err := request.Policy.Validate(); err != nil {
		return err
	}
	stepName := types.StepName(request.StepName)
	if request.Gate == nil && !runconfine.IsConfinableStep(stepName) {
		return fmt.Errorf("run-confinement-worker: step %q is not confinable", stepName)
	}
	return nil
}

func runIsolatedRunRequest(request isolatedRunInput, decoder *json.Decoder, output io.Writer) error {
	if err := validateIsolatedRunRequest(request); err != nil {
		return err
	}
	state, err := os.ReadFile("/nm/state.db")
	if err != nil {
		return err
	}
	if err := os.WriteFile("/tmp/control.db", state, 0600); err != nil {
		return err
	}
	database, err := db.Open("/tmp/control.db")
	if err != nil {
		return err
	}
	defer database.Close()
	closeServices, err := testisolation.ServePrivateServices()
	if err != nil {
		return err
	}
	defer closeServices()
	// Invocation-local dummy, not a provider credential: the host bridge
	// strips it and supplies the configured host-side key itself.
	if err := os.WriteFile("/tmp/model-client-key", []byte("isolated-client"), 0600); err != nil {
		return err
	}
	profile := request.Config.AgentConfig["deck"]
	profile.Model = request.Model
	ag, err := agent.NewWithOptions(types.AgentDeck, "/nm/deck", []string{"--base-url", testisolation.ModelURL, "--api-key-file", "/tmp/model-client-key"}, agent.Options{
		DisableProjectSettings: request.Config.DisableProjectSettings, Profile: profile,
	})
	if err != nil {
		return err
	}
	defer ag.Close()
	encoder := json.NewEncoder(output)
	var mu sync.Mutex
	emit := func(event isolatedStepEvent) error { mu.Lock(); defer mu.Unlock(); return encoder.Encode(event) }
	stepName := types.StepName(request.StepName)
	workerAgent := pipeline.InstrumentIsolatedStepAgent(stepName, isolatedRunGateAgent{ag, stepName}, database, request.Run.ID, request.Round,
		func(inv db.AgentInvocation) { emit(isolatedStepEvent{Kind: "invocation", Invocation: &inv}) },
		func() { emit(isolatedStepEvent{Kind: "activity"}) })
	sctx := request.stepContext(database, workerAgent, decoder, emit)
	step, err := request.step()
	if err != nil {
		return err
	}
	return executeIsolatedWorkerStep(step, sctx, emit)
}

// The worker recreates each step's ordinary gate boundary without
// reconstructing the host's agent factory, hooks, environment or credentials.
type isolatedRunGateAgent struct {
	agent.Agent
	step types.StepName
}

func (a isolatedRunGateAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.Prompt = gateguidance.PromptBoundary(string(a.step)) + opts.Prompt
	return a.Agent.Run(ctx, opts)
}

// confinedRunConfig narrows the serialized config to the fields the confinable
// steps can read. Agent arguments are fixed by the worker, so a global --hook
// or --base-url cannot bypass the model channel, and credentials, forge
// profiles and agent path overrides never enter the namespace.
func confinedRunConfig(src *config.Config) config.Config {
	cfg := config.Config{Agent: types.AgentDeck, Commands: config.Commands{Prepare: src.Commands.Prepare, Test: src.Commands.Test, Lint: src.Commands.Lint, Format: src.Commands.Format},
		Test: src.Test, Commit: src.Commit, PR: config.PR{BaseBranch: src.PR.BaseBranch},
		IgnorePatterns: src.IgnorePatterns, ProtectedPaths: src.ProtectedPaths, AgentTimeout: src.AgentTimeout,
		TestAgentTimeout: src.TestAgentTimeout, ReviewAgentTimeout: src.ReviewAgentTimeout, AgentConfig: src.AgentConfig, DisableProjectSettings: src.DisableProjectSettings,
		Review: src.Review, Document: src.Document, AutoFix: src.AutoFix}
	cfg.Review.Routes = config.ReviewRoutesRaw{}
	return cfg
}

func confinedRepo(src *db.Repo) *db.Repo {
	return &db.Repo{ID: src.ID, WorkingPath: src.WorkingPath, DefaultBranch: src.DefaultBranch}
}

func (request isolatedRunInput) stepContext(database *db.DB, workerAgent agent.Agent, decoder *json.Decoder, emit func(isolatedStepEvent) error) *pipeline.StepContext {
	sctx := &pipeline.StepContext{Ctx: context.Background(), Run: request.Run, Repo: request.Repo, Config: &request.Config, WorkDir: request.WorkDir, GateDir: request.GateDir,
		EvidenceDir: request.EvidenceDir, StepResultID: request.StepResultID, Fixing: request.Fixing, PreviousFindings: request.PreviousFindings,
		DeferredFindings: request.DeferredFindings, UserIntent: request.UserIntent, IntentSource: request.IntentSource, DB: database, Agent: workerAgent,
		FixWithoutRereview: request.FixWithoutRereview, ReviewStartingHeadSHA: request.ReviewStartingHeadSHA,
		UncertifiedFromSHA: request.UncertifiedFromSHA, UncertifiedToSHA: request.UncertifiedToSHA, UncertifiedSourceRunID: request.UncertifiedSourceRunID, UncertifiedPriorRounds: request.UncertifiedPriorRounds,
		Shared: &pipeline.RunShared{}, IsolatedTestWorker: true, PriorBranchDecisions: request.PriorBranchDecisions, PriorBranchDecisionsTruncated: request.PriorBranchDecisionsTruncated,
	}
	configureIsolatedWorkerContext(sctx, decoder, emit)
	return sctx
}

func (request isolatedRunInput) step() (pipeline.Step, error) {
	stepName := types.StepName(request.StepName)

	var step pipeline.Step
	switch gate := request.Gate; {
	case gate != nil:
		step = &CustomGateStep{Gate: *gate}
	case stepName == types.StepReview:
		step = &ReviewStep{}
	case stepName == types.StepTest:
		step = &TestStep{}
	case stepName == types.StepDocument:
		step = &DocumentStep{}
	case stepName == types.StepLint:
		step = &LintStep{}
	default:
		return nil, fmt.Errorf("run-confinement-worker: step %q is not confinable", stepName)
	}
	return step, nil
}
