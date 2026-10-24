package steps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type isolatedProtocolStep struct {
	execute func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (isolatedProtocolStep) Name() types.StepName { return types.StepTest }
func (s isolatedProtocolStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.execute(ctx)
}

func TestIsolatedWorkerCustodyAcknowledgementAndTerminalEvents(t *testing.T) {
	for _, scenario := range []string{"success", "timeout", "rejected", "disconnected"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", head)
			if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("repair"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "repair.txt")
			gitCmd(t, dir, "commit", "-m", "repair")
			fixed := gitCmd(t, dir, "rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(dir, "staged-only.txt"), []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "staged-only.txt")
			if err := os.Remove(filepath.Join(dir, "staged-only.txt")); err != nil {
				t.Fatal(err)
			}
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "deck"}, dir, base, head, config.Commands{})
			ack := `""`
			if scenario == "rejected" {
				ack = `"custody refused"`
			}
			if scenario == "disconnected" {
				ack = ""
			}
			var events []isolatedStepEvent
			emit := func(event isolatedStepEvent) error {
				if event.Kind == "head" {
					if sctx.Run.HeadSHA != head || event.Text != fixed || len(event.Pack) == 0 {
						t.Fatalf("head advanced before acknowledgement: %+v", event)
					}
				}
				events = append(events, event)
				return nil
			}
			configureIsolatedWorkerContext(sctx, json.NewDecoder(strings.NewReader(ack)), emit)
			step := isolatedProtocolStep{execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				ctx.Log("log")
				ctx.LogChunk("chunk")
				ctx.LogFile("file")
				if err := ctx.RecordIsolatedHead(fixed); err != nil {
					return nil, err
				}
				ctx.Run.HeadSHA = fixed
				if scenario == "timeout" {
					return nil, pipeline.ErrAgentTimeout
				}
				return &pipeline.StepOutcome{}, nil
			}}
			if err := executeIsolatedWorkerStep(step, sctx, emit); err != nil {
				t.Fatal(err)
			}
			if len(events) != 5 {
				t.Fatalf("events lost: %+v", events)
			}
			for i, kind := range []string{"log", "chunk", "file", "head"} {
				if events[i].Kind != kind {
					t.Fatalf("event %d: %+v", i, events[i])
				}
			}
			terminal := events[4]
			if terminal.Index == nil || !strings.Contains(string(terminal.Index.Entries), "staged-only.txt") || len(terminal.Index.Pack) == 0 {
				t.Fatalf("terminal event lost index: %+v", terminal)
			}
			if scenario == "success" {
				if terminal.Kind != "result" || terminal.Outcome == nil {
					t.Fatalf("result lost: %+v", terminal)
				}
			} else {
				if terminal.Kind != "error" || terminal.Error == nil {
					t.Fatalf("failure became success: %+v", terminal)
				}
				err := terminal.Error.Restore()
				if scenario == "timeout" && !errors.Is(err, pipeline.ErrAgentTimeout) || scenario == "rejected" && !strings.Contains(err.Error(), "custody refused") || scenario == "disconnected" && !strings.Contains(err.Error(), io.EOF.Error()) {
					t.Fatalf("cause lost: %v", err)
				}
			}
			expected := fixed
			if scenario == "rejected" || scenario == "disconnected" {
				expected = head
			}
			if sctx.Run.HeadSHA != expected {
				t.Fatalf("acknowledgement violated: %s", sctx.Run.HeadSHA)
			}
			receiver := filepath.Join(t.TempDir(), "receiver")
			gitCmd(t, dir, "clone", "--no-hardlinks", dir, receiver)
			if err := testisolation.ImportGitIndex(context.Background(), receiver, terminal.Index); err != nil {
				t.Fatal(err)
			}
			if got := gitCmd(t, receiver, "show", ":staged-only.txt"); got != "retained" {
				t.Fatalf("staged blob lost: %s", got)
			}
		})
	}
}
