package testisolation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ModelConfig stays in the daemon. Only its constrained Unix bridge crosses the
// boundary; neither this config nor the credential file is mounted in Test.
type ModelConfig struct{ URL, KeyFile, Model string }

func ModelConfigFromEnvironment() ModelConfig {
	return ModelConfig{URL: os.Getenv("NM_TEST_MODEL_URL"), KeyFile: os.Getenv("NM_TEST_MODEL_KEY_FILE"), Model: os.Getenv("NM_TEST_MODEL")}
}
func (c ModelConfig) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "/v1" && u.Path != "/v1/") {
		return fmt.Errorf("test-isolation-model-config: NM_TEST_MODEL_URL must name an explicit http(s) /v1 endpoint")
	}
	if c.KeyFile == "" || !filepath.IsAbs(c.KeyFile) {
		return fmt.Errorf("test-isolation-model-config: NM_TEST_MODEL_KEY_FILE must be an absolute host-only credential reference")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("test-isolation-model-config: NM_TEST_MODEL is required")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("test-isolation-model-config: plaintext transport is permitted only to an explicit loopback IP")
		}
	}
	return nil
}

func modelHandler(c ModelConfig) (http.Handler, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("test-isolation-model-credential: cannot read configured key file")
	}
	secret := strings.TrimSpace(string(key))
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return nil, fmt.Errorf("test-isolation-model-credential: invalid key file")
	}
	// No ambient proxy, cookies, redirects, or caller-selected destinations.
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	destination := strings.TrimRight(c.URL, "/") + "/responses"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" || r.URL.IsAbs() {
			http.Error(w, "model operation refused", 403)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "model request exceeds limit", 413)
			return
		}
		var selection struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &selection); err != nil || selection.Model != c.Model {
			http.Error(w, "model selection refused", 403)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, destination, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "model request refused", 400)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		response, err := client.Do(req)
		if err != nil {
			http.Error(w, "model transport failed", 502)
			return
		}
		defer response.Body.Close()
		// A redirect must not become a new authority, including in the client.
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			http.Error(w, "model redirect refused", 502)
			return
		}
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32<<10)
		for {
			n, err := response.Body.Read(buffer)
			if n > 0 {
				if _, e := w.Write(buffer[:n]); e != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	})
	return &modelBridgeHandler{Handler: handler, transport: transport}, nil
}

type modelBridgeHandler struct {
	http.Handler
	transport *http.Transport
}

func (h *modelBridgeHandler) Close() { h.transport.CloseIdleConnections() }

// ModelFrontend runs inside the namespace over one inherited, preconnected
// Unix STREAM channel. There is no host socket path or general proxy socket.
func ModelFrontend(connection net.Conn) http.Handler {
	var used atomic.Bool
	client := &http.Client{Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if used.Swap(true) {
			return nil, fmt.Errorf("model channel closed")
		}
		return connection, nil
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" {
			http.Error(w, "model operation refused", 403)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://model/v1/responses", http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		response, err := client.Do(req)
		if err != nil {
			http.Error(w, "model bridge unavailable", 502)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		// ReverseProxy's streaming behavior without exposing its arbitrary routing.
		buffer := make([]byte, 32<<10)
		for {
			n, err := response.Body.Read(buffer)
			if n > 0 {
				if _, e := w.Write(buffer[:n]); e != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					return
				}
				return
			}
		}
	})
}
