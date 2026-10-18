package steps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The worker gets only its run's state and the Test-relevant config, not global
// credentials, forge profiles, agent command overrides, daemon DB, or sockets.
type isolatedTestInput struct {
	Run                                                                                                       *db.Run
	Repo                                                                                                      *db.Repo
	Config                                                                                                    config.Config
	Model                                                                                                     string
	WorkDir, GateDir, EvidenceDir, StepResultID, PreviousFindings, DeferredFindings, UserIntent, IntentSource string
	Fixing                                                                                                    bool
	PriorBranchDecisions                                                                                      []*db.BranchDecisionRound
	PriorBranchDecisionsTruncated                                                                             bool
}
type isolatedTestEvent struct {
	Kind    string
	Text    string
	Outcome *pipeline.StepOutcome
}

func executeIsolatedTest(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return sctx.RunIsolatedTestRepair(func(markStarted func(int) error) (*pipeline.StepOutcome, error) {
		return executeIsolatedTestInvocation(sctx, markStarted)
	})
}

func executeIsolatedTestInvocation(sctx *pipeline.StepContext, markStarted func(int) error) (*pipeline.StepOutcome, error) {
	if sctx.Agent == nil || sctx.Agent.Name() != "deck" {
		return nil, fmt.Errorf("test-isolation-adapter: Deck is required")
	}
	if err := testisolation.Check(); err != nil {
		return nil, err
	}
	if sctx.EvidenceDir == "" {
		return nil, fmt.Errorf("test-isolation-storage: evidence directory is required")
	}
	if err := os.MkdirAll(sctx.EvidenceDir, 0700); err != nil {
		return nil, err
	}
	root := filepath.Join(filepath.Dir(sctx.EvidenceDir), "."+sctx.Run.ID+"-test-isolation")
	// A recovered invocation gets a fresh snapshot/socket. Product database
	// storage lived only in the dead namespace's tmpfs and cannot be resumed.
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state.db")
	if err := sctx.DB.ExportTestSnapshot(state, sctx.Run.ID); err != nil {
		return nil, err
	}
	worker, err := os.Executable()
	if err != nil {
		return nil, err
	}
	deck, err := exec.LookPath("deck")
	if err != nil {
		return nil, fmt.Errorf("test-isolation-adapter: Deck binary unavailable")
	}
	if override := sctx.Config.AgentPathOverride["deck"]; override != "" {
		deck = override
	}
	deck, err = filepath.Abs(deck)
	if err != nil {
		return nil, err
	}
	postgresRoot, err := testisolation.PostgresRoot()
	if err != nil {
		return nil, err
	}
	model := testisolation.ModelConfigFromEnvironment()
	invocation, err := testisolation.Start(sctx.Ctx, testisolation.Options{
		Worker: worker, Deck: deck, State: state, WorkDir: sctx.WorkDir, GateDir: sctx.GateDir, EvidenceDir: sctx.EvidenceDir, ScratchParent: root, PostgresRoot: postgresRoot, Model: model,
	})
	if err != nil {
		return nil, err
	}
	defer invocation.Close()
	cmd := invocation.Command
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return nil, fmt.Errorf("test-isolation-confinement: %w", err)
	}
	defer shellenv.TerminateShellCommandGroup(cmd)
	if markStarted != nil {
		if err := markStarted(cmd.Process.Pid); err != nil {
			cmd.Cancel()
			cmd.Wait()
			return nil, err
		}
	}
	// Do not serialize config fields unrelated to Test. Agent arguments are fixed
	// below, so a global --hook or --base-url cannot bypass the model channel.
	cfg := config.Config{Agent: types.AgentDeck, Commands: config.Commands{Prepare: sctx.Config.Commands.Prepare, Test: sctx.Config.Commands.Test}, Test: sctx.Config.Test, Commit: sctx.Config.Commit, PR: config.PR{BaseBranch: sctx.Config.PR.BaseBranch},
		IgnorePatterns: sctx.Config.IgnorePatterns, ProtectedPaths: sctx.Config.ProtectedPaths, AgentTimeout: sctx.Config.AgentTimeout,
		TestAgentTimeout: sctx.Config.TestAgentTimeout, AgentConfig: sctx.Config.AgentConfig, DisableProjectSettings: sctx.Config.DisableProjectSettings}
	input := isolatedTestInput{Run: sctx.Run, Repo: &db.Repo{ID: sctx.Repo.ID, WorkingPath: sctx.Repo.WorkingPath, DefaultBranch: sctx.Repo.DefaultBranch}, Config: cfg, Model: model.Model, WorkDir: sctx.WorkDir, GateDir: sctx.GateDir, EvidenceDir: sctx.EvidenceDir,
		StepResultID: sctx.StepResultID, Fixing: sctx.Fixing, PreviousFindings: sctx.PreviousFindings, DeferredFindings: sctx.DeferredFindings,
		UserIntent: sctx.UserIntent, IntentSource: sctx.IntentSource, PriorBranchDecisions: sctx.PriorBranchDecisions, PriorBranchDecisionsTruncated: sctx.PriorBranchDecisionsTruncated}
	encoder := json.NewEncoder(in)
	if err := encoder.Encode(input); err != nil {
		cmd.Cancel()
		cmd.Wait()
		return nil, err
	}
	decoder := json.NewDecoder(bufio.NewReader(out))
	var outcome *pipeline.StepOutcome
	var workerErr error
	finished := false
	for {
		var event isolatedTestEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			workerErr = fmt.Errorf("test-isolation-protocol: invalid worker event: %w", err)
			cmd.Cancel()
			break
		}
		if finished {
			workerErr = fmt.Errorf("test-isolation-protocol: event after result")
			cmd.Cancel()
			break
		}
		switch event.Kind {
		case "log":
			sctx.Log(event.Text)
		case "chunk":
			if sctx.LogChunk != nil {
				sctx.LogChunk(event.Text)
			}
		case "file":
			if sctx.LogFile != nil {
				sctx.LogFile(event.Text)
			}
		case "head":
			if len(event.Text) != 40 && len(event.Text) != 64 {
				workerErr = fmt.Errorf("test-isolation-protocol: invalid head")
			} else {
				workerErr = sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, event.Text)
			}
			ack := ""
			if workerErr != nil {
				ack = "durable head acknowledgement failed"
			} else {
				sctx.Run.HeadSHA = event.Text
			}
			if err := encoder.Encode(ack); err != nil {
				workerErr = err
				cmd.Cancel()
			}
		case "result":
			finished = true
			outcome = event.Outcome
		case "error":
			finished = true
			workerErr = fmt.Errorf("isolated Test: %s", event.Text)
		default:
			workerErr = fmt.Errorf("test-isolation-protocol: unknown worker event")
			cmd.Cancel()
		}
		if workerErr != nil && !finished {
			break
		}
	}
	in.Close()
	waitErr := cmd.Wait()
	if workerErr != nil {
		return nil, workerErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("test-isolation-worker: %w: %s", waitErr, stderr.String())
	}
	if !finished || outcome == nil {
		return nil, fmt.Errorf("test-isolation-worker: missing final result")
	}
	return outcome, nil
}

// RunIsolatedTestWorker is an internal binary entrypoint. Its fixed filesystem
// inputs only exist in the bubblewrap namespace. No ordinary CLI launch calls
// TestStep with IsolatedTestWorker set, and no ambient marker disables isolation.
func RunIsolatedTestWorker(input io.Reader, output io.Writer) error {
	if err := testisolation.RestrictWorkerNetwork(); err != nil {
		return err
	}
	decoder := json.NewDecoder(input)
	var request isolatedTestInput
	if err := decoder.Decode(&request); err != nil {
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
	// This is an invocation-local dummy, not a provider credential. The host
	// bridge strips it and supplies the configured host-side key itself.
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
	emit := func(event isolatedTestEvent) error { mu.Lock(); defer mu.Unlock(); return encoder.Encode(event) }
	sctx := &pipeline.StepContext{Ctx: context.Background(), Run: request.Run, Repo: request.Repo, Config: &request.Config, WorkDir: request.WorkDir, GateDir: request.GateDir,
		EvidenceDir: request.EvidenceDir, StepResultID: request.StepResultID, Fixing: request.Fixing, PreviousFindings: request.PreviousFindings,
		DeferredFindings: request.DeferredFindings, UserIntent: request.UserIntent, IntentSource: request.IntentSource, DB: database, Agent: isolatedTestGateAgent{ag},
		Shared: &pipeline.RunShared{}, IsolatedTestWorker: true, PriorBranchDecisions: request.PriorBranchDecisions, PriorBranchDecisionsTruncated: request.PriorBranchDecisionsTruncated,
		Log:      func(text string) { emit(isolatedTestEvent{Kind: "log", Text: text}) },
		LogChunk: func(text string) { emit(isolatedTestEvent{Kind: "chunk", Text: text}) },
		LogFile:  func(text string) { emit(isolatedTestEvent{Kind: "file", Text: text}) },
		RecordIsolatedHead: func(head string) error {
			if err := emit(isolatedTestEvent{Kind: "head", Text: head}); err != nil {
				return err
			}
			var acknowledgement string
			if err := decoder.Decode(&acknowledgement); err != nil {
				return err
			}
			if acknowledgement != "" {
				return fmt.Errorf("%s", acknowledgement)
			}
			return nil
		},
	}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		return emit(isolatedTestEvent{Kind: "error", Text: err.Error()})
	}
	return emit(isolatedTestEvent{Kind: "result", Outcome: outcome})
}

// The worker recreates the ordinary Test gate's prompt boundary, without
// reconstructing the host's agent factory, hooks, environment, or credentials.
type isolatedTestGateAgent struct{ agent.Agent }

func (a isolatedTestGateAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.Prompt = gateguidance.PromptBoundary(string(types.StepTest)) + opts.Prompt
	return a.Agent.Run(ctx, opts)
}
