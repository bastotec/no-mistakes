package cli

import (
	"bytes"
	"testing"
)

func TestAxiRefreshPublicationRequiresExplicitBinding(t *testing.T) {
	for _, args := range [][]string{nil, {"--run", "some-run"}, {"--run", "some-run", "--pr", "https://github.com/test/repo/pull/42"}, {"--run", "some-run", "--head", "abcdef", "--pr", "https://github.com/test/repo/pull/42"}} {
		cmd := newAxiRefreshPublicationCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted incomplete binding: %v", args)
		}
	}
}
