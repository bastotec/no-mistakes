package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateSendsWireProtocolAndParsesAnswers(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"breaking":{"type":"boolean","probability":0.87},"bug":{"type":"boolean","probability":0.11}},"usage":{},"rounding":{},"warnings":[]}`))
	}))
	defer srv.Close()

	client := NewClient("test-key-123", WithEndpoint(srv.URL), WithModel("typesafe-ai/jev"))
	res, err := client.Evaluate(context.Background(), "a diff", AdvisoryQuestions())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if gotPath != "/" {
		t.Errorf("request path = %q, want /", gotPath)
	}
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer test-key-123" {
		t.Errorf("Authorization = %q", auth)
	}
	if v := gotHeaders.Get("ai-gateway-protocol-version"); v != "0.0.1" {
		t.Errorf("ai-gateway-protocol-version = %q", v)
	}
	if v := gotHeaders.Get("ai-gateway-auth-method"); v != "api-key" {
		t.Errorf("ai-gateway-auth-method = %q", v)
	}
	if v := gotHeaders.Get("ai-evaluation-model-specification-version"); v != "4" {
		t.Errorf("ai-evaluation-model-specification-version = %q", v)
	}
	if v := gotHeaders.Get("ai-model-id"); v != "typesafe-ai/jev" {
		t.Errorf("ai-model-id = %q", v)
	}

	if state, _ := gotBody["state"].(string); state != "a diff" {
		t.Errorf("state = %v", gotBody["state"])
	}
	questions, ok := gotBody["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions payload = %#v", gotBody["questions"])
	}
	if len(questions) != len(AdvisoryQuestions()) {
		t.Errorf("questions count = %d, want %d", len(questions), len(AdvisoryQuestions()))
	}
	breaking, ok := questions["breaking"].(map[string]any)
	if !ok {
		t.Fatalf("breaking question = %#v", questions["breaking"])
	}
	if typ, _ := breaking["type"].(string); typ != "boolean" {
		t.Errorf("breaking type = %v", breaking["type"])
	}
	if instr, _ := breaking["instructions"].(string); instr == "" {
		t.Error("breaking instructions empty")
	}
	criteria, ok := breaking["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("breaking criteria = %#v", breaking["criteria"])
	}
	if criteria["true"] == "" || criteria["false"] == "" {
		t.Errorf("breaking criteria incomplete: %#v", criteria)
	}

	if p, ok := res.Probability("breaking"); !ok || p != 0.87 {
		t.Errorf("breaking probability = %v ok=%v", p, ok)
	}
	if p, ok := res.Probability("bug"); !ok || p != 0.11 {
		t.Errorf("bug probability = %v ok=%v", p, ok)
	}
	if _, ok := res.Probability("absent"); ok {
		t.Error("absent question reported as answered")
	}
}

func TestEvaluateLargeSuccessBodyIsNotTruncated(t *testing.T) {
	pad := strings.Repeat("x", 64*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"breaking":{"type":"boolean","probability":0.9}},"usage":{},"rounding":{},"warnings":["` + pad + `"]}`))
	}))
	defer srv.Close()

	client := NewClient("test-key-123", WithEndpoint(srv.URL))
	res, err := client.Evaluate(context.Background(), "a diff", AdvisoryQuestions())
	if err != nil {
		t.Fatalf("Evaluate on a >2 KiB success body: %v", err)
	}
	if p, ok := res.Probability("breaking"); !ok || p != 0.9 {
		t.Errorf("breaking probability = %v ok=%v", p, ok)
	}
}

func TestEvaluateErrorOmitsKey(t *testing.T) {
	const key = "super-secret-key-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"AI Gateway authentication failed"}`))
	}))
	defer srv.Close()

	client := NewClient(key, WithEndpoint(srv.URL))
	_, err := client.Evaluate(context.Background(), "state", AdvisoryQuestions())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error lacks status: %v", err)
	}
}

func TestEvaluateSurfacesTransportFailureWithoutURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	client := NewClient("k", WithEndpoint(url))
	_, err := client.Evaluate(context.Background(), "state", AdvisoryQuestions())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), url) {
		t.Errorf("error embeds endpoint URL: %v", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error is not an unavailability: %v", err)
	}
}

func TestEvaluateRejectsBadInputsBeforeNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	client := NewClient("k", WithEndpoint(srv.URL))

	if _, err := client.Evaluate(context.Background(), "", AdvisoryQuestions()); err == nil {
		t.Error("empty state accepted")
	}
	if _, err := client.Evaluate(context.Background(), "s", nil); err == nil {
		t.Error("no questions accepted")
	}
	dup := []Question{{Name: "a", Instructions: "i", TrueCriteria: "t", FalseCriteria: "f"}, {Name: "a", Instructions: "i2", TrueCriteria: "t", FalseCriteria: "f"}}
	if _, err := client.Evaluate(context.Background(), "s", dup); err == nil {
		t.Error("duplicate question names accepted")
	}
	if called {
		t.Error("network was reached for invalid input")
	}
}

func TestEvaluateWithoutKeyFailsClosed(t *testing.T) {
	client := NewClient("")
	if _, err := client.Evaluate(context.Background(), "s", AdvisoryQuestions()); err != ErrNoKey {
		t.Errorf("error = %v, want ErrNoKey", err)
	}
}

func TestEvaluateNoAnswersIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{},"warnings":[]}`))
	}))
	defer srv.Close()
	client := NewClient("k", WithEndpoint(srv.URL))
	if _, err := client.Evaluate(context.Background(), "s", AdvisoryQuestions()); err == nil || !strings.Contains(err.Error(), "no answers") {
		t.Errorf("error = %v, want no-answers failure", err)
	}
}

func TestResolveKeyEnvWinsThenSecretsFile(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.WriteFile(secrets, []byte("# comment\nexport OTHER=1\nDOC_KEY=\"file-key-1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOC_KEY", "env-key")
	got, err := ResolveKey("DOC_KEY", secrets)
	if err != nil {
		t.Fatalf("ResolveKey env: %v", err)
	}
	if got != "env-key" {
		t.Errorf("key = %q, want env value", got)
	}

	os.Unsetenv("DOC_KEY")
	got, err = ResolveKey("DOC_KEY", secrets)
	if err != nil {
		t.Fatalf("ResolveKey file: %v", err)
	}
	if got != "file-key-1" {
		t.Errorf("key = %q, want parsed file value with quotes stripped", got)
	}

	got, err = ResolveKey("ABSENT_VAR", secrets)
	if err != nil {
		t.Fatalf("ResolveKey absent: %v", err)
	}
	if got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
}

func TestResolveKeyMissingFileIsEmptyNotError(t *testing.T) {
	got, err := ResolveKey("X", filepath.Join(t.TempDir(), "nope"))
	if err != nil || got != "" {
		t.Errorf("missing file: key=%q err=%v", got, err)
	}
}

func TestResolveKeyExpandsHomeTilde(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows; set
	// both so the tilde resolves into this test's temp dir on every platform.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	secrets := filepath.Join(home, ".secrets")
	if err := os.WriteFile(secrets, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveKey("K", "~/.secrets")
	if err != nil {
		t.Fatalf("ResolveKey tilde: %v", err)
	}
	if got != "v" {
		t.Errorf("key = %q", got)
	}
}

func TestIsGeminiModel(t *testing.T) {
	for _, m := range []string{"google/gemini-2.5-pro", "gemini-2.0-flash", "GOOGLE/GEMINI-1.5", "vertex/gemini-2.5", "google/gemini-flash-latest"} {
		if !IsGeminiModel(m) {
			t.Errorf("IsGeminiModel(%q) = false", m)
		}
	}
	for _, m := range []string{"typesafe-ai/jev", "openai/gpt-4o", "anthropic/claude-sonnet-4", "geminite"} {
		if IsGeminiModel(m) {
			t.Errorf("IsGeminiModel(%q) = true", m)
		}
	}
}
