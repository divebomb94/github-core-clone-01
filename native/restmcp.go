package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
)

type mcpProperty struct {
	name string
	kind string
}

type mcpDefinition struct {
	name        string
	aliases     []string
	description string
	method      string
	path        string
	properties  []mcpProperty
	required    []string
}

var mcpDefinitions = []mcpDefinition{
	{
		"create_repo", []string{"create_repository", "repo_create", "repos_create", "repository_create"},
		"Create a repository under the authenticated viewer.", "POST", "/api/v3/user/repos",
		[]mcpProperty{{"name", "string"}, {"description", "string"}, {"private", "boolean"}}, []string{"name"},
	},
	{
		"get_repo", []string{"get_repository", "repo_get", "repos_get", "repository_get"},
		"Get a repository.", "GET", "/api/v3/repos/:owner/:repo",
		[]mcpProperty{{"owner", "string"}, {"repo", "string"}}, []string{"owner", "repo"},
	},
	{
		"create_issue", []string{"issue_create", "issues_create"},
		"Create an issue in a repository.", "POST", "/api/v3/repos/:owner/:repo/issues",
		[]mcpProperty{{"owner", "string"}, {"repo", "string"}, {"title", "string"}, {"body", "string"}}, []string{"owner", "repo", "title"},
	},
	{
		"list_issues", []string{"issues_list", "issue_list"},
		"List repository issues, optionally filtered by state.", "GET", "/api/v3/repos/:owner/:repo/issues",
		[]mcpProperty{{"owner", "string"}, {"repo", "string"}, {"state", "string"}}, []string{"owner", "repo"},
	},
	{
		"create_pull_request", []string{"pull_request_create", "pulls_create", "create_pr", "pr_create"},
		"Open a pull request from head into base.", "POST", "/api/v3/repos/:owner/:repo/pulls",
		[]mcpProperty{{"owner", "string"}, {"repo", "string"}, {"title", "string"}, {"head", "string"}, {"base", "string"}, {"body", "string"}}, []string{"owner", "repo", "title", "head", "base"},
	},
	{
		"merge_pull_request", []string{"pull_request_merge", "pulls_merge", "merge_pr", "pr_merge"},
		"Merge an open pull request.", "PUT", "/api/v3/repos/:owner/:repo/pulls/:number/merge",
		[]mcpProperty{{"owner", "string"}, {"repo", "string"}, {"pull_number", "integer"}}, []string{"owner", "repo", "pull_number"},
	},
}

// Tool schemas are independent of request and repository state.
var mcpTools = func() []any {
	tools := []any{}
	for _, definition := range mcpDefinitions {
		properties := make(map[string]any, len(definition.properties))
		for _, property := range definition.properties {
			properties[property.name] = map[string]string{"type": property.kind}
		}
		for _, name := range append([]string{definition.name}, definition.aliases...) {
			tools = append(tools, map[string]any{
				"name": name, "description": definition.description,
				"inputSchema": map[string]any{"type": "object", "properties": properties, "required": definition.required},
			})
		}
	}
	return tools
}()

func (a *App) serveMcpRoutes(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) bool {
	if r.Method != http.MethodPost || len(parts) != 1 || parts[0] != "mcp" {
		return false
	}
	var id any
	switch value := body["id"].(type) {
	case string, float64:
		id = value
	}
	rpcError := func(code int, message string) {
		sendJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	result := func(value any) {
		sendJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": value})
	}
	method, validMethod := body["method"].(string)
	if body == nil || body["jsonrpc"] != "2.0" || !validMethod {
		rpcError(-32600, "Invalid Request")
		return true
	}
	params, _ := body["params"].(map[string]any)
	switch method {
	case "initialize":
		version := params["protocolVersion"]
		if version == nil {
			version = "2025-11-25"
		}
		result(map[string]any{
			"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "github-forge", "version": "1.0.0"},
		})
		return true
	case "notifications/initialized":
		if _, exists := body["id"]; !exists {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
	case "tools/list":
		result(map[string]any{"tools": mcpTools})
		return true
	case "tools/call":
	default:
		rpcError(-32601, "Method not found")
		return true
	}
	if method != "tools/call" {
		rpcError(-32601, "Method not found")
		return true
	}
	var definition *mcpDefinition
	name, _ := params["name"].(string)
	for i := range mcpDefinitions {
		candidate := &mcpDefinitions[i]
		if name == candidate.name {
			definition = candidate
		}
		for _, alias := range candidate.aliases {
			if name == alias {
				definition = candidate
			}
		}
	}
	if definition == nil {
		rpcError(-32601, "Unknown tool")
		return true
	}
	args, ok := params["arguments"].(map[string]any)
	if !ok || args == nil {
		rpcError(-32602, "Invalid arguments")
		return true
	}
	for _, property := range definition.properties {
		required := false
		for _, key := range definition.required {
			required = required || key == property.name
		}
		value, exists := args[property.name]
		if !exists && !required {
			continue
		}
		valid := false
		switch property.kind {
		case "string":
			text, ok := value.(string)
			valid = ok && (!required || strings.Trim(text, "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff") != "")
		case "boolean":
			_, valid = value.(bool)
		case "integer":
			number, ok := value.(float64)
			valid = ok && number >= 1 && number <= 9007199254740991 && math.Trunc(number) == number
		}
		if !valid {
			rpcError(-32602, "Invalid argument: "+property.name)
			return true
		}
	}
	text, isError := a.callMcpTool(r, definition, args)
	result(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": isError})
	return true
}

func (a *App) callMcpTool(r *http.Request, definition *mcpDefinition, args map[string]any) (text string, isError bool) {
	defer func() {
		if cause := recover(); cause != nil {
			message := "Tool failed"
			if err, ok := cause.(error); ok {
				message = err.Error()
			}
			data, _ := json.Marshal(map[string]string{"message": message})
			text, isError = string(data), true
		}
	}()
	owner, _ := args["owner"].(string)
	repo, _ := args["repo"].(string)
	number := ""
	if value, ok := args["pull_number"].(float64); ok {
		number = strconv.FormatFloat(value, 'f', -1, 64)
	}
	// Split the template before substitution so slashes in arguments remain
	// part of a route parameter rather than becoming route separators.
	parts := strings.Split(strings.TrimPrefix(definition.path, "/"), "/")
	for i, part := range parts {
		switch part {
		case ":owner":
			parts[i] = url.PathEscape(owner)
		case ":repo":
			parts[i] = url.PathEscape(repo)
		case ":number":
			parts[i] = number
		}
	}
	state, ok := args["state"].(string)
	if !ok {
		state = "open"
	}
	requestURL := "/api/v3/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues?state=" + url.QueryEscape(state) + "&per_page=100"
	request := &http.Request{
		Method: definition.method, Header: r.Header.Clone(), Host: r.Host,
		URL:        &url.URL{Path: "/api/v3/repos/" + owner + "/" + repo + "/issues", RawQuery: "state=" + url.QueryEscape(state) + "&per_page=100"},
		RequestURI: requestURL,
	}
	capture := httptest.NewRecorder()
	var handled bool
	switch definition.name {
	case "create_repo", "get_repo":
		handled = a.serveRepoRoutes(capture, request, parts, args)
	case "create_issue", "list_issues":
		handled = a.serveIssueRoutes(capture, request, parts, args)
	case "create_pull_request", "merge_pull_request":
		handled = a.servePullRoutes(capture, request, parts, args)
	}
	if !handled {
		return `{"message":"Tool failed"}`, true
	}
	return capture.Body.String(), capture.Code >= 400
}
