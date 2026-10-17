package github

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestVerifyTestPreview_FailsClosed(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, payload := range []string{
		`{}`, `{"isDraft":false,"state":"OPEN","headRefOid":"` + head + `","autoMergeRequest":null}`,
		`{"isDraft":true,"state":"CLOSED","headRefOid":"` + head + `","autoMergeRequest":null}`,
		`{"isDraft":true,"state":"OPEN","headRefOid":"stale","autoMergeRequest":null}`,
		`{"isDraft":true,"state":"OPEN","headRefOid":"` + head + `","autoMergeRequest":{}}`,
		`{"isDraft":true,"state":"OPEN","headRefOid":"` + head + `"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			var calls [][]string
			h := New(recordingCmdFactory(payload, &calls), nil, "github.com", "test/repo")
			if err := h.VerifyTestPreview(context.Background(), &scm.PR{Number: "99"}, head); err == nil {
				t.Fatal("unsafe/incomplete PR accepted")
			}
		})
	}
	var calls [][]string
	h := New(recordingCmdFactory(`{"isDraft":true,"state":"OPEN","headRefOid":"`+head+`","autoMergeRequest":null}`, &calls), nil, "github.com", "test/repo")
	if err := h.VerifyTestPreview(context.Background(), &scm.PR{Number: "99"}, head); err != nil {
		t.Fatal(err)
	}
}

func TestTestPreviewURL_NeverReturnsStaleOrInactiveDeployment(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct{ name, deployments, status, want string }{
		{"current", `[{"id":1,"sha":"` + head + `","environment":"PR Preview"}]`, `[{"state":"success","environment_url":"https://preview.example.test/current"}]`, "https://preview.example.test/current"},
		{"stale", `[{"id":1,"sha":"old","environment":"PR Preview"}]`, `[{"state":"success","environment_url":"https://preview.example.test/old"}]`, ""},
		{"inactive", `[{"id":1,"sha":"` + head + `","environment":"PR Preview"}]`, `[{"state":"inactive"},{"state":"success","environment_url":"https://preview.example.test/old"}]`, ""},
		{"production", `[{"id":1,"sha":"` + head + `","environment":"production"}]`, `[{"state":"success","environment_url":"https://example.test"}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			cmd := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				payload := tc.deployments
				if strings.Contains(strings.Join(args, " "), "/statuses") {
					payload = tc.status
				}
				return recordingCmdFactory(payload, &calls)(ctx, name, args...)
			}
			h := New(cmd, nil, "github.com", "test/repo")
			got, err := h.TestPreviewURL(context.Background(), head)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
			if !strings.Contains(strings.Join(calls[0], " "), "sha="+head) {
				t.Fatal("deployment lookup not pinned to head")
			}
		})
	}
}
