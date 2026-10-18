package testisolation

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelBridgePinsAuthorityAndRejectsRedirects(t *testing.T) {
	var requests atomic.Int32
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer host-credential" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.URL.Path != "/v1/responses" {
			t.Errorf("unconfined authority: %v %s", r.Header, r.URL)
		}
		http.Redirect(w, r, other.URL, 307)
	}))
	defer upstream.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("host-credential"), 0600)
	handler, err := modelHandler(ModelConfig{URL: upstream.URL + "/v1", KeyFile: key, Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/responses", `{"model":"fixture-model"}`, 403},
		{"POST", "/v1/admin", `{"model":"fixture-model"}`, 403},
		{"POST", "/v1/responses?url=external", `{"model":"fixture-model"}`, 403},
		{"CONNECT", "/v1/responses", `{"model":"fixture-model"}`, 403},
		{"POST", "/v1/responses", `{"model":"unapproved-model"}`, 403},
		{"POST", "/v1/responses", `{"model":"fixture-model"}`, 502},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer attacker")
		request.Header.Set("Cookie", "ambient=attacker")
		request.Header.Set("X-Api-Key", "attacker")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("%+v: %d %s", test, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "host-credential") {
			t.Fatal("credential exposed in error")
		}
	}
	if requests.Load() != 1 || redirected.Load() != 0 {
		t.Fatalf("unexpected upstream calls=%d redirects=%d", requests.Load(), redirected.Load())
	}
}
func TestModelConfigRefusesImplicitOrUnscopedTransport(t *testing.T) {
	for _, config := range []ModelConfig{
		{}, {URL: "http://127.0.0.1:1/v1", Model: "model"}, {URL: "https://example.invalid/v1", KeyFile: "key"},
		{URL: "http://192.0.2.1/v1", KeyFile: "key", Model: "model"},
		{URL: "https://user:pass@example.invalid/v1", KeyFile: "key", Model: "model"},
		{URL: "https://example.invalid/v1?route=other", KeyFile: "key", Model: "model"},
	} {
		if config.Validate() == nil {
			t.Fatalf("accepted implicit/unscoped config: %+v", config)
		}
	}
	handler, err := modelHandler(ModelConfig{URL: "http://127.0.0.1:1/v1", KeyFile: filepath.Join(t.TempDir(), "absent"), Model: "model"})
	if handler != nil || err == nil || !strings.Contains(err.Error(), "test-isolation-model-credential") {
		t.Fatalf("missing credential did not refuse: %v", err)
	}
}
