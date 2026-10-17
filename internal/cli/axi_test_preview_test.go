package cli

import (
	"bytes"
	"testing"
)

func TestAxiTestPreviewRequiresExplicitRunAndFullHead(t *testing.T) {
	for _, args := range [][]string{nil, {"--run", "some-run"}, {"--head", "abcdef"}, {"--run", "some-run", "--head", "abcdef"}} {
		cmd := newAxiTestPreviewCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("missing operator evidence accepted: %v", args)
		}
	}
}
