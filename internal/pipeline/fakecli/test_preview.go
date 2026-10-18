package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func fakeTestPreviewGH(args []string) {
	joined := strings.Join(args, " ")
	path := os.Getenv("FAKE_CLI_PR_BODY_FILE")
	_, exists := os.Stat(path)
	if title, ok := fakeCLIFlagValue(args, "--title"); ok {
		_ = os.WriteFile(path+".title", []byte(title), 0600)
	}
	if title, err := os.ReadFile(path + ".title"); err == nil {
		_ = os.Setenv("FAKE_CLI_PR_TITLE", string(title))
	}
	_, otherOpen := os.Stat(path + ".other-open")
	switch {
	case strings.HasPrefix(joined, "auth status"):
	case strings.HasPrefix(joined, "pr list"):
		if exists != nil {
			fmt.Print("[]")
		} else if otherOpen == nil {
			fmt.Print(`[{"number":100,"url":"https://github.com/test/repo/pull/100","baseRefName":"release"},{"number":99,"url":"https://github.com/test/repo/pull/99","baseRefName":"main"}]`)
		} else {
			fmt.Print(`[{"number":99,"url":"https://github.com/test/repo/pull/99","baseRefName":"main"}]`)
		}
	case strings.HasPrefix(joined, "pr create"):
		if !strings.Contains(joined, "--draft") {
			os.Exit(2)
		}
		fakeGHStorePRBody(args)
		if os.Getenv("FAKE_CLI_PREVIEW_CREATE_RESPONSE_LOST") == "1" {
			os.Exit(1)
		}
		fmt.Print("https://github.com/test/repo/pull/99")
	case strings.HasPrefix(joined, "pr ready"):
		_ = os.WriteFile(path+".ready", []byte("ready"), 0600)
	case strings.Contains(joined, "--json state"):
		state := os.Getenv("FAKE_CLI_PR_STATE")
		if state == "ERROR" {
			os.Exit(1)
		}
		if state == "" {
			state = "OPEN"
		}
		fmt.Print(state)
	case strings.Contains(joined, "--json isDraft,state,headRefOid,autoMergeRequest"):
		_, ready := os.Stat(path + ".ready")
		head := os.Getenv("FAKE_CLI_PR_HEAD_SHA")
		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"isDraft": ready != nil, "state": "OPEN", "headRefOid": head, "autoMergeRequest": nil})
	case strings.HasPrefix(joined, "pr view 100 "):
		fmt.Print(`{"title":"Other PR","body":"Unmarked PR targeting release"}`)
	case strings.HasPrefix(joined, "pr view") || strings.HasPrefix(joined, "pr edit"):
		fakeGHHandlePRContentCommands(args, joined)
	case strings.HasPrefix(joined, "api"):
		if strings.Contains(joined, "/pulls") {
			if !strings.Contains(joined, "--paginate --slurp") || !strings.Contains(joined, "state=open") || !strings.Contains(joined, "-f head=") {
				os.Exit(2)
			}
			pages := [][]map[string]interface{}{{}}
			if exists == nil {
				if otherOpen == nil {
					pages[0] = append(pages[0], map[string]interface{}{"body": "Unmarked PR targeting release"})
					pages = append(pages, []map[string]interface{}{})
				}
				body, err := os.ReadFile(path)
				if err != nil {
					os.Exit(1)
				}
				last := len(pages) - 1
				pages[last] = append(pages[last], map[string]interface{}{"body": string(body)})
			}
			_ = json.NewEncoder(os.Stdout).Encode(pages)
		} else if strings.Contains(joined, "/deployments/") {
			fmt.Print(`[{"state":"success","environment_url":"https://preview.example.test/current"}]`)
		} else if strings.Contains(joined, "/deployments") {
			fmt.Printf(`[{"id":1,"sha":%q,"environment":"PR preview"}]`, os.Getenv("FAKE_CLI_PR_HEAD_SHA"))
		} else {
			fmt.Print(`{}`)
		}
	default:
		fmt.Fprintln(os.Stderr, "unsupported preview fake:", joined)
		os.Exit(2)
	}
	os.Exit(0)
}
