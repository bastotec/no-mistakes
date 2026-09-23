package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Deck emits JSONL on stdout, including a terminal run_finished or run_failed
// event. Only run_finished.output is a verdict; text_delta is progress, not a
// fallback answer. Runs are ephemeral: review must never inherit fixer state.
// Deck currently has no verified project-settings suppression or effort flag.
type deckAgent struct {
	subprocessContext
	bin       string
	extraArgs []string
}

func (*deckAgent) Name() string { return "deck" }
func (*deckAgent) Close() error { return nil }

func (a *deckAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	args := append([]string{"run"}, a.extraArgs...)
	args = append(args, "--ephemeral", "-")
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt += "\n\nReturn only a final JSON object matching this JSON Schema (no Markdown or prose):\n" + string(opts.JSONSchema)
	}
	cmd.Stdin = strings.NewReader(prompt)
	shellenv.ConfigureShellCommand(cmd)
	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "deck"))
	if err != nil {
		return nil, fmt.Errorf("deck start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "deck", pid)
	// Drain stderr concurrently, retaining only a diagnostic excerpt.
	stderr := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(started.stderr, 4096))
		_, _ = io.Copy(io.Discard, started.stderr)
		stderr <- strings.TrimSpace(string(b))
	}()
	result, parseErr := parseDeckEvents(started.stdout, opts)
	var waitErr error
	if parseErr != nil {
		waitErr = started.waitAfterParseError(parseErr)
	} else {
		waitErr = started.wait()
	}
	detail := <-stderr
	err = parseErr
	if waitErr != nil {
		err = fmt.Errorf("deck exited: %w: %s", waitErr, detail)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	emitAgentExited(opts, "deck", pid, err)
	return result, err
}

func parseDeckEvents(r io.Reader, opts RunOpts) (*Result, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var output, model string
	var usage TokenUsage
	finished := false
	for scanner.Scan() {
		var event struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			Output       string `json:"output"`
			Error        string `json:"error"`
			Model        string `json:"model"`
			Input        *int   `json:"input_tokens"`
			OutputTokens *int   `json:"output_tokens"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return resultFromUsage(usage), fmt.Errorf("deck event: %w", err)
		}
		switch event.Type {
		case "run_started":
			model = event.Model
		case "text_delta":
			if opts.OnChunk != nil {
				opts.OnChunk(event.Text)
			}
		case "usage", "run_finished":
			if event.Input != nil && event.OutputTokens != nil {
				usage = TokenUsage{InputTokens: *event.Input, OutputTokens: *event.OutputTokens, Reported: true}
			}
			if event.Type == "run_finished" {
				output, finished = event.Output, true
			}
		case "run_failed":
			return resultFromUsage(usage), fmt.Errorf("deck run failed: %s", event.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		return resultFromUsage(usage), fmt.Errorf("deck stream: %w", err)
	}
	if !finished {
		return resultFromUsage(usage), fmt.Errorf("deck stream ended without run_finished")
	}
	result, err := finalizeTextResult("deck", output, opts.JSONSchema, usage)
	if result != nil {
		result.Model, result.Provider = model, "deck"
	}
	return result, err
}
