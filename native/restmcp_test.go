package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMcpProtocol(t *testing.T) {
	app := NewApp("secret")
	for _, test := range []struct {
		body   string
		status int
		code   float64
	}{
		{`[]`, 200, -32600},
		{`{"jsonrpc":"2.0","method":42}`, 200, -32600},
		{`{"jsonrpc":"2.0","method":"unknown"}`, 200, -32601},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, 0},
		{`{"jsonrpc":"2.0","method":"notifications/initialized","id":null}`, 200, -32601},
		{`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"unknown"}}`, 200, -32601},
		{`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_repo","arguments":[]}}`, 200, -32602},
		{`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_repo","arguments":{"name":" "}}}`, 200, -32602},
	} {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer secret")
		capture := httptest.NewRecorder()
		app.ServeHTTP(capture, req)
		if capture.Code != test.status {
			t.Fatalf("%s: status %d", test.body, capture.Code)
		}
		if test.status == 202 {
			if capture.Body.Len() != 0 {
				t.Fatal("notification returned a body")
			}
			continue
		}
		var reply map[string]any
		if err := json.Unmarshal(capture.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply["error"].(map[string]any)["code"] != test.code || reply["id"] != nil {
			t.Fatalf("%s: %v", test.body, reply)
		}
	}
}

func TestMcpRestDispatch(t *testing.T) {
	app := NewApp("secret")
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "call", "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer secret")
		capture := httptest.NewRecorder()
		app.ServeHTTP(capture, req)
		var reply struct {
			ID     string `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(capture.Body.Bytes(), &reply); err != nil || reply.ID != "call" || len(reply.Result.Content) != 1 {
			t.Fatalf("unexpected reply: %s (%v)", capture.Body.String(), err)
		}
		return reply.Result.Content[0].Text, reply.Result.IsError
	}
	if text, failed := call("repository_create", map[string]any{"name": "mcp"}); failed {
		t.Fatal(text)
	}
	for i := 0; i < 35; i++ {
		if text, failed := call("issues_create", map[string]any{"owner": ViewerLogin, "repo": "mcp", "title": fmt.Sprint(i)}); failed {
			t.Fatal(text)
		}
	}
	app.store.Repos[repoKey(ViewerLogin, "mcp")].Items[1].(*Issue).State = "closed"
	for state, count := range map[string]int{"open": 34, "closed": 1, "all": 35} {
		text, failed := call("issue_list", map[string]any{"owner": ViewerLogin, "repo": "mcp", "state": state})
		var issues []any
		if err := json.Unmarshal([]byte(text), &issues); failed || err != nil || len(issues) != count {
			t.Fatalf("state %s: %s (%v)", state, text, err)
		}
	}
	if _, failed := call("get_repository", map[string]any{"owner": ViewerLogin, "repo": "missing"}); !failed {
		t.Fatal("REST error was not reported as tool error")
	}
}
