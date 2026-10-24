package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Counterfactual: without the run pin a busy Document invocation is killed
// by the ordinary budget, even though its last output was recent.
func TestDocumentTimeoutPinControlsBothAgentSeamsOnlyForDocument(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			t.Run(map[bool]string{true: "direct", false: "shared"}[direct]+map[bool]string{true: "-pinned", false: "-default"}[pinned], func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				if pinned {
					if err := database.PinDocumentTimeout(run.ID, 2*time.Second); err != nil {
						t.Fatal(err)
					}
				}
				ag := &hangingAgent{name: "busy", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					finish := time.NewTimer(80 * time.Millisecond)
					defer finish.Stop()
					for {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-ticker.C:
							if opts.OnChunk != nil {
								opts.OnChunk("still working")
							}
						case <-finish.C:
							return &agent.Result{Text: "done"}, nil
						}
					}
				}}
				cfg := &config.Config{AgentTimeout: 20 * time.Millisecond}
				step := &adaptiveCallStep{name: types.StepDocument, fn: func(s *StepContext) (*StepOutcome, error) {
					var err error
					if direct {
						_, err = s.Agent.Run(s.Ctx, agent.RunOpts{Prompt: "work"})
					} else {
						_, err = s.RunAgent(agent.RunOpts{Prompt: "work"})
					}
					return &StepOutcome{}, err
				}}
				steps := []Step{step}
				if pinned {
					steps = append(steps, &adaptiveCallStep{name: types.StepLint, fn: func(s *StepContext) (*StepOutcome, error) {
						if s.Config.AgentTimeout != cfg.AgentTimeout {
							t.Fatal("Document timeout leaked to Lint")
						}
						return &StepOutcome{}, nil
					}})
				}
				exec := NewExecutor(database, p, cfg, ag, steps, nil)
				err := exec.Execute(context.Background(), run, repo, t.TempDir())
				if (err == nil) != pinned {
					t.Fatalf("pinned=%v err=%v", pinned, err)
				}
				if cfg.AgentTimeout != 20*time.Millisecond {
					t.Fatal("run pin changed shared config")
				}
			})
		}
	}
}
