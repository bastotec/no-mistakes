package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The monitor remains pending while the separately head-scoped refresh reads
// the configured observation. This models checks changing between observations
// without launching agents, workflow reruns, or a real forge request.
func fakePublicationRefreshGH(args []string) {
	joined := strings.Join(args, " ")
	transcript, err := os.OpenFile(os.Getenv("FAKE_CLI_PR_BODY_FILE")+".commands", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
	err = json.NewEncoder(transcript).Encode(args)
	_ = transcript.Close()
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
	marker := os.Getenv("FAKE_CLI_PR_BODY_FILE") + ".refresh-reading"
	switch {
	case strings.HasPrefix(joined, "pr view ") && strings.Contains(joined, "--json title,body"):
		// Refresh reads the body before and after observing checks. Between
		// those reads, serve the requested observation; outside them the
		// ordinary monitor keeps waiting on a pending check.
		if _, err := os.Stat(marker); err == nil {
			_ = os.Remove(marker)
		} else if err := os.WriteFile(marker, nil, 0o600); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
	case strings.Contains(joined, "api") && strings.Contains(joined, "graphql"):
		if _, err := os.Stat(marker); err != nil {
			printFakeCommitChecks(`[{"name":"monitor","state":"IN_PROGRESS","bucket":"pending"}]`, args)
			os.Exit(0)
		}
		if os.Getenv("FAKE_CLI_REFRESH_ERROR") == "1" {
			fmt.Fprint(os.Stderr, "fixture checks unavailable")
			os.Exit(1)
		}
		printFakeCommitChecks(os.Getenv("FAKE_CLI_REFRESH_CHECKS"), args)
		os.Exit(0)
	case strings.HasPrefix(joined, "pr edit "):
		// Reject every non-body mutation, including labels, title, base and
		// merge authority. Assertions consume the resulting body and RPC reply.
		if len(args) != 7 || args[2] != "42" || args[3] != "--repo" || args[4] != "test/repo" || args[5] != "--body-file" || args[6] != "-" {
			fmt.Fprint(os.Stderr, "fixture refuses non-body edit")
			os.Exit(1)
		}
	}
	fakeCIGHHandler(args)
}
