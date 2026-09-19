// Package jev is a minimal client for the Jev evaluation model
// (typesafe-ai/jev) served through the Vercel AI Gateway's evaluation-model
// endpoint. It exists to power the pipeline's advisory Jev signal: a diff is
// submitted as the evaluation state together with a fixed set of typed boolean
// questions, and the model answers each with a calibrated probability.
//
// The package owns the wire protocol only. What the answers mean, when to ask,
// and what happens on failure are decisions of the caller (the advisory
// pipeline step); this client deliberately never retries, never logs, and never
// embeds the API key in an error or URL.
//
// Protocol (captured against the gateway as used by the TypeScript AI SDK's
// experimental_evaluate over @ai-sdk/gateway):
//
//	POST {endpoint}                      (default https://ai-gateway.vercel.sh/v4/ai/evaluation-model)
//	Authorization: Bearer <key>
//	ai-gateway-protocol-version: 0.0.1
//	ai-gateway-auth-method: api-key
//	ai-evaluation-model-specification-version: 4
//	ai-model-id: typesafe-ai/jev
//	Content-Type: application/json
//	{"state":"<text>","questions":{"<name>":{"type":"boolean",
//	 "instructions":"...","criteria":{"true":"...","false":"..."}}}}
//	-> {"answers":{"<name>":{"type":"boolean","probability":0.87}},
//	   "usage":{...},"rounding":{...},"warnings":[...]}
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultEndpoint is the Vercel AI Gateway evaluation-model URL.
	DefaultEndpoint = "https://ai-gateway.vercel.sh/v4/ai/evaluation-model"
	// DefaultModel is the Jev evaluation model served by the gateway.
	DefaultModel = "typesafe-ai/jev"
	// DefaultTimeout bounds one evaluation call.
	DefaultTimeout = 30 * time.Second
	// DefaultKeyEnv is the environment variable consulted for the gateway
	// key when the caller has not named one explicitly.
	DefaultKeyEnv = "AI_GATEWAY_API_KEY"
	// DefaultSecretsFile is parsed (never executed) for the key variable
	// when the environment does not carry it.
	DefaultSecretsFile = "~/.secrets"
	// maxResponseBody bounds how much of an error response is kept for the
	// error message, so a hostile or broken endpoint cannot flood a step log.
	maxResponseBody = 2048
	// maxSuccessResponseBody bounds a successful (200) response body. A
	// valid answer set is tiny; this generous ceiling exists only so a
	// runaway endpoint cannot flood memory, never to truncate a response
	// the signal could have used.
	maxSuccessResponseBody = 1 << 20

	// MaxStateBytes is the default cap on the state (diff) sent for one
	// evaluation. It owns the value: the configured default
	// (config.DefaultJevMaxDiffBytes), the unconfigured fallback, and the
	// tally harness all reference this constant.
	MaxStateBytes = 64 * 1024
)

// Question is one typed boolean question evaluated against a state. The
// instructions and criteria are submitted verbatim; names must be unique
// within one call because answers are keyed by them.
type Question struct {
	Name          string
	Instructions  string
	TrueCriteria  string
	FalseCriteria string
}

// Answer is the model's response for one question.
type Answer struct {
	// Probability is the calibrated probability the question's answer is
	// true, in [0,1].
	Probability float64
}

// Result is a successful evaluation.
type Result struct {
	Answers map[string]Answer
}

// Probability returns the answer for one question and whether the model
// answered it at all.
func (r *Result) Probability(name string) (float64, bool) {
	if r == nil {
		return 0, false
	}
	a, ok := r.Answers[name]
	if !ok {
		return 0, false
	}
	return a.Probability, true
}

// geminiPattern matches model ids routed to any Gemini provider. Jev calls
// must never use a Gemini model, regardless of configuration.
var geminiPattern = regexp.MustCompile(`(?i)(^|/)gemini(-|/|$)|^google/`)

// IsGeminiModel reports whether the model id names a Gemini model. The
// advisory signal refuses such models outright; the refusal reason names the
// model but never a key.
func IsGeminiModel(model string) bool {
	return geminiPattern.MatchString(strings.TrimSpace(model))
}

// Client talks to one gateway endpoint with one key. It is safe for
// concurrent use. The key is held only here: it is sent as the Authorization
// header and never returned in errors or included in URLs.
type Client struct {
	endpoint string
	model    string
	key      string
	http     *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithEndpoint overrides the gateway endpoint (tests point this at an
// httptest server).
func WithEndpoint(url string) Option {
	return func(c *Client) {
		if strings.TrimSpace(url) != "" {
			c.endpoint = strings.TrimSpace(url)
		}
	}
}

// WithModel overrides the model id.
func WithModel(model string) Option {
	return func(c *Client) {
		if strings.TrimSpace(model) != "" {
			c.model = strings.TrimSpace(model)
		}
	}
}

// WithHTTPClient overrides the HTTP client (timeout injection in tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// NewClient builds a Client for the given key. An empty key yields a client
// that fails every call with ErrNoKey rather than reaching the network.
func NewClient(key string, opts ...Option) *Client {
	c := &Client{
		endpoint: DefaultEndpoint,
		model:    DefaultModel,
		key:      strings.TrimSpace(key),
		http:     &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ErrNoKey is returned when a client has no key configured.
var ErrNoKey = errors.New("jev: no gateway key configured")

type wireQuestion struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     map[string]any `json:"criteria"`
}

type wireRequest struct {
	State     string                  `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type        string  `json:"type"`
	Probability float64 `json:"probability"`
}

type wireResponse struct {
	Answers  map[string]wireAnswer `json:"answers"`
	Warnings []string              `json:"warnings"`
}

// Evaluate submits state and questions in one round trip and returns the
// answers keyed by question name. It does not retry: the caller owns retry
// policy, and the advisory step deliberately has none (skip-and-report).
func (c *Client) Evaluate(ctx context.Context, state string, questions []Question) (*Result, error) {
	if c.key == "" {
		return nil, ErrNoKey
	}
	if strings.TrimSpace(state) == "" {
		return nil, errors.New("jev: empty state")
	}
	if len(questions) == 0 {
		return nil, errors.New("jev: no questions")
	}
	reqBody := wireRequest{
		State:     state,
		Questions: make(map[string]wireQuestion, len(questions)),
	}
	for _, q := range questions {
		name := strings.TrimSpace(q.Name)
		if name == "" {
			return nil, errors.New("jev: question with empty name")
		}
		if _, dup := reqBody.Questions[name]; dup {
			return nil, fmt.Errorf("jev: duplicate question name %q", name)
		}
		if strings.TrimSpace(q.Instructions) == "" {
			return nil, fmt.Errorf("jev: question %q has empty instructions", name)
		}
		if strings.TrimSpace(q.TrueCriteria) == "" || strings.TrimSpace(q.FalseCriteria) == "" {
			return nil, fmt.Errorf("jev: question %q has incomplete criteria", name)
		}
		reqBody.Questions[name] = wireQuestion{
			Type:         "boolean",
			Instructions: q.Instructions,
			Criteria:     map[string]any{"true": q.TrueCriteria, "false": q.FalseCriteria},
		}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ai-gateway-protocol-version", "0.0.1")
	req.Header.Set("ai-gateway-auth-method", "api-key")
	req.Header.Set("ai-evaluation-model-specification-version", "4")
	req.Header.Set("ai-model-id", c.model)

	resp, err := c.http.Do(req)
	if err != nil {
		// Wrap without the underlying error's URL: net/http errors can embed
		// the full request URL; it never carries the key, but keep messages
		// minimal and stable anyway.
		return nil, fmt.Errorf("jev: gateway unreachable: %w", errors.Unwrap(err))
	}
	defer resp.Body.Close()
	readCap := int64(maxResponseBody)
	if resp.StatusCode == http.StatusOK {
		readCap = maxSuccessResponseBody
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, readCap))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("jev: gateway status %d: %s", resp.StatusCode, msg)
	}
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	if len(wire.Answers) == 0 {
		return nil, errors.New("jev: response has no answers")
	}
	result := &Result{Answers: make(map[string]Answer, len(wire.Answers))}
	for name, a := range wire.Answers {
		result.Answers[name] = Answer{Probability: a.Probability}
	}
	return result, nil
}

// Verdict summarizes a result into one word shared by every consumer of
// the advisory signal: "flagged" when any answered probability is at or
// above threshold, "uncertain" when any lands in the gray zone (above
// 1-threshold) or a question went unanswered, else "clear". The advisory
// step and the tally harness both read this so a measured tally and a live
// run can never disagree about what a set of answers means.
func Verdict(result *Result, questions []Question, threshold float64) string {
	anyUncertain, anyFlagged := false, false
	for _, q := range questions {
		p, ok := result.Probability(q.Name)
		if !ok {
			anyUncertain = true
			continue
		}
		switch {
		case p >= threshold:
			anyFlagged = true
		case p > 1-threshold:
			anyUncertain = true
		}
	}
	switch {
	case anyFlagged:
		return "flagged"
	case anyUncertain:
		return "uncertain"
	default:
		return "clear"
	}
}

// ResolveKey finds the gateway key by reference: the named environment
// variable first, then a parsed (never executed) secrets file holding
// `VAR=value` or `export VAR=value` lines. Quotes around the value are
// stripped. The returned key is the secret itself; callers must not log it.
// An empty envVar or secretsPath falls back to the defaults.
func ResolveKey(envVar, secretsPath string) (string, error) {
	envVar = strings.TrimSpace(envVar)
	if envVar == "" {
		envVar = DefaultKeyEnv
	}
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v, nil
	}
	path := strings.TrimSpace(secretsPath)
	if path == "" {
		path = DefaultSecretsFile
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("jev: resolve secrets file: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("jev: read secrets file: %w", err)
	}
	return keyValue(string(data), envVar), nil
}

// keyValue extracts VAR's value from shell-style assignment lines without
// executing anything. Returns "" when absent.
func keyValue(text, name string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name+"=") {
			continue
		}
		value := strings.TrimPrefix(line, name+"=")
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		return value
	}
	return ""
}
