package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Deck emits JSONL on stdout, including a terminal run_finished or run_failed
// event. Only run_finished.output is a verdict; text_delta is progress, not a
// fallback answer. Runs are ephemeral: review must never inherit fixer state.
type deckAgent struct {
	subprocessContext
	bin       string
	extraArgs []string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// Run passes --no-project-context so Deck ignores the target repo's own
	// AGENTS.md chain, .agents/skills and .agents/deck.hooks.json.
	disableProjectSettings bool
	// supportsNoProjectContext records whether this Deck binary's
	// `run --help` lists --no-project-context; probed only under the opt-out.
	supportsNoProjectContext bool
}

func newDeckAgent(bin string, extraArgs []string, disableProjectSettings bool, sub subprocessContext) *deckAgent {
	a := &deckAgent{bin: bin, extraArgs: extraArgs, disableProjectSettings: disableProjectSettings, subprocessContext: sub}
	if disableProjectSettings {
		a.supportsNoProjectContext = deckSupportsNoProjectContext(bin)
	}
	return a
}

func (*deckAgent) Name() string { return "deck" }

// ReportsAgentAttempts reports that Run emits OnAttempt for each try itself
// (through runWithRetry), so wrapping routers must not emit another.
func (*deckAgent) ReportsAgentAttempts() bool { return true }
func (*deckAgent) Close() error               { return nil }

// NeutralizesGateInstructions reports whether Deck is launched with the target
// repo's project context ignored. It is meaningful only under the opt-out
// (disableProjectSettings): the gate only consults it when the repo opted out.
// `deck run --no-project-context` drops the project's AGENTS.md chain,
// <repo>/.agents/skills, <repo>/.agents/deck.hooks.json and its chat launcher,
// while keeping the operator's ~/.agents files - the claude
// `--setting-sources user` and codex `project_doc_max_bytes=0` shape. Deck has
// no flag that re-enables the project context, so an operator-pinned
// --no-project-context only preserves it. The knob is admitted only when the
// installed binary's `run --help` lists it: an older Deck would reject the
// flag, and the gate refuses it up front rather than failing every step.
func (a *deckAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && a.supportsNoProjectContext
}

// buildArgs returns the Deck argv for one ephemeral run. Under the opt-out the
// suppression flag follows the verb, before user extras, unless the operator
// already pinned it (clap rejects a repeated flag).
func (a *deckAgent) buildArgs(tuning *InvocationTuning) []string {
	extra := a.extraArgs
	if tuning != nil {
		extra = withValueFlag(extra, tuning.Model, "--model", "-m")
		extra = withValueFlag(extra, tuning.Effort, "--effort")
		if tuning.MaxTurns > 0 {
			extra = withValueFlag(extra, strconv.Itoa(tuning.MaxTurns), "--max-turns")
		}
		if tuning.DeadlineSecs > 0 {
			extra = withValueFlag(extra, strconv.Itoa(tuning.DeadlineSecs), "--deadline-secs")
		}
	}
	args := make([]string, 0, len(extra)+4)
	args = append(args, "run")
	if a.disableProjectSettings && !deckArgsContain(extra, deckNoProjectContextFlag) {
		args = append(args, deckNoProjectContextFlag)
	}
	args = append(args, extra...)
	return append(args, "--ephemeral", "-")
}

// withValueFlag returns args with every spelling of one value flag (`--flag v`
// or `--flag=v`) replaced by a single `names[0] value`. Deck (clap) rejects a
// repeated flag, and other CLIs take the first or the last, so an override must
// replace the operator's value rather than sit beside it. An empty value
// leaves args untouched.
func withValueFlag(args []string, value string, names ...string) []string {
	if value == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		matched := false
		for _, name := range names {
			if args[i] == name {
				matched = true
				i++ // skip the value
				break
			}
			if strings.HasPrefix(args[i], name+"=") {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, args[i])
		}
	}
	return append(out, names[0], value)
}

// deckMaxRetries bounds the in-place reruns after a transient gateway
// failure. A Deck run replays its prompt in a fresh ephemeral session against
// the same worktree, retaining whatever the previous attempt left on disk
// rather than rolling it back to a fresh checkout.
const deckMaxRetries = 2

// deckRunFailure preserves the native run_failed origin separately from output
// validation and process diagnostics, which can quote gateway error text.
type deckRunFailure struct {
	message string
}

func (e *deckRunFailure) Error() string { return "deck run failed: " + e.message }

// deckGatewayStatusRE reads the HTTP status Deck appends to a gateway failure.
var deckGatewayStatusRE = regexp.MustCompile(`^model gateway request failed \(http (\d{3})\)`)

// classifyDeckTransient retries only native gateway failures deemed transient:
// a request that failed without a status (a dropped upstream stream, such as
// proxai's codex_stream_failed), a 5xx, or a 429. Any other 4xx is a request
// the gateway refused - a bad route, effort, credential or payload - and is
// never retried. Nothing that is not a gateway failure is retried either: a
// model that answered with a refusal or prose would only answer the same way
// again.
func classifyDeckTransient(err error) (string, bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	var failure *deckRunFailure
	if !errors.As(err, &failure) {
		return "", false
	}
	msg := strings.ToLower(failure.message)
	if msg == "model gateway request failed" || strings.HasPrefix(msg, "model gateway request failed:") {
		return "gateway request failed", true
	}
	if m := deckGatewayStatusRE.FindStringSubmatch(msg); m != nil && (m[1] == "429" || strings.HasPrefix(m[1], "5")) {
		return "gateway http " + m[1], true
	}
	return "", false
}

func (a *deckAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "deck", opts, deckMaxRetries, classifyDeckTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *deckAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	cmd := exec.CommandContext(ctx, a.bin, a.buildArgs(opts.Tuning)...)
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
	stream := &deckStreamReader{Reader: started.stdout}
	result, parseErr := parseDeckEvents(stream, opts)
	if parseErr != nil && !stream.ended {
		// An early protocol error must not leave Deck running. Keep stderr
		// open so its diagnostic drains, rather than racing closePipes.
		started.terminate()
		_, _ = io.Copy(io.Discard, started.stdout)
	}
	// EOF validation errors (notably no run_finished) are not grounds to
	// kill the child: it may still be writing its startup failure to stderr.
	// waitAfterParseError would also replace the real exit status with the
	// parser error. Retain both, with the process failure first.
	waitErr := started.wait()
	detail := <-stderr
	err = parseErr
	if waitErr != nil {
		exitErr := fmt.Errorf("deck exited: %w", waitErr)
		if detail != "" {
			exitErr = fmt.Errorf("%w: %s", exitErr, detail)
		}
		err = errors.Join(exitErr, parseErr)
	} else if err != nil && detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	emitAgentExited(opts, "deck", pid, err)
	return result, err
}

// deckStreamReader distinguishes EOF validation from an early parse failure.
type deckStreamReader struct {
	io.Reader
	ended bool
}

func (r *deckStreamReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.ended = true
	}
	return n, err
}

func parseDeckEvents(r io.Reader, opts RunOpts) (*Result, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var output, model string
	var usage TokenUsage
	finished := false
	for scanner.Scan() {
		var event struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			Output       json.RawMessage `json:"output"`
			Error        string          `json:"error"`
			Model        string          `json:"model"`
			Input        *int            `json:"input_tokens"`
			OutputTokens *int            `json:"output_tokens"`
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
				if finished {
					return resultFromUsage(usage), fmt.Errorf("deck duplicate run_finished")
				}
				if len(event.Output) == 0 || string(event.Output) == "null" {
					return resultFromUsage(usage), fmt.Errorf("deck terminal output must be a string")
				}
				// Tool events also carry output, but as a structured object.
				// Decode only the terminal answer as text.
				if err := json.Unmarshal(event.Output, &output); err != nil {
					return resultFromUsage(usage), fmt.Errorf("deck terminal output: %w", err)
				}
				finished = true
			}
		case "run_failed":
			return resultFromUsage(usage), &deckRunFailure{message: event.Error}
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

const deckNoProjectContextFlag = "--no-project-context"

func deckArgsContain(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// deckProbeTimeout bounds the `deck run --help` feature probe.
const deckProbeTimeout = 10 * time.Second

type deckProbeKey struct {
	path    string
	size    int64
	modTime time.Time
}

// deckProbes caches feature probes per binary identity, so a redeployed Deck
// (new size or mtime at the same path) is probed again.
var deckProbes sync.Map

// deckSupportsNoProjectContext reports whether the Deck binary lists
// --no-project-context in `deck run --help`. Deck's crate version does not
// track features, so the help text is the feature check. Any failure (missing
// binary, timeout, non-zero exit) reports false, which fails the gate closed.
func deckSupportsNoProjectContext(bin string) bool {
	path, err := exec.LookPath(bin)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	key := deckProbeKey{path: path, size: info.Size(), modTime: info.ModTime()}
	if cached, ok := deckProbes.Load(key); ok {
		return cached.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deckProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "run", "--help").Output()
	supported := err == nil && strings.Contains(string(out), deckNoProjectContextFlag)
	deckProbes.Store(key, supported)
	return supported
}
