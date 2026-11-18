package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

type App struct {
	token     string
	store     *Store
	requestID atomic.Uint64
}

func NewApp(token string) *App {
	return &App{token: token, store: NewStore()}
}

func sendJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"message":"Internal Server Error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strconv.FormatUint(a.requestID.Add(1), 10)
	w.Header().Set("X-Request-Id", id)
	w.Header().Set("X-GitHub-Request-Id", id)
	path := strings.SplitN(r.RequestURI, "?", 2)[0]
	if path == "" {
		path = r.URL.EscapedPath()
	}
	var gitData []byte
	var rawBody []byte
	var body map[string]any
	var bodyIsArray bool
	switch r.Method {
	case "POST", "PUT", "PATCH", "DELETE":
		data, err := io.ReadAll(r.Body)
		rawBody = data
		gitBody := strings.HasSuffix(path, "/git-upload-pack") || strings.HasSuffix(path, "/git-receive-pack")
		if err != nil || (!gitBody && len(data) > 0 && !json.Valid(data)) {
			if path == "/mcp" {
				sendJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}})
			} else {
				sendJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
			}
			return
		}
		if gitBody {
			gitData = data
		}
		_ = json.Unmarshal(data, &body)
		trimmed := bytes.TrimSpace(data)
		bodyIsArray = len(trimmed) > 0 && trimmed[0] == '['
	}
	if path != "/healthz" && r.Header.Get("Authorization") != "Bearer "+a.token {
		sendJSON(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	// The TypeScript router discards empty segments, including trailing slashes.
	parts := strings.FieldsFunc(path, func(c rune) bool { return c == '/' })
	route := "/" + strings.Join(parts, "/")
	if r.Method == http.MethodGet {
		switch route {
		case "/healthz":
			sendJSON(w, 200, map[string]bool{"ok": true})
			return
		case "/api/v3":
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			sendJSON(w, 200, map[string]string{
				"current_user_url": "/api/v3/user",
				"repository_url":   "/api/v3/repos/{owner}/{repo}",
				"issue_url":        "/api/v3/issues",
				"pull_request_url": "/api/v3/repos/{owner}/{repo}/pulls",
				"commit_url":       "/api/v3/repos/{owner}/{repo}/commits",
				"content_url":      "/api/v3/repos/{owner}/{repo}/contents",
				"label_url":        "/api/v3/labels/{name}",
				"notification_url": "/api/v3/notifications",
			})
			return
		case "/api/v3/meta":
			sendJSON(w, 200, map[string]string{"installed_version": "1.0.0"})
			return
		case "/api/v3/user":
			sendJSON(w, 200, map[string]any{"login": ViewerLogin, "id": ViewerID, "name": ViewerName, "email": ViewerEmail, "type": "User"})
			return
		}
	}
	if a.serveMcpRoutes(w, r, parts, body) {
		return
	}
	if a.serveGraphqlRoutes(w, r, parts, rawBody) {
		return
	}
	if a.serveGitRoutes(w, r, parts, gitData) {
		return
	}
	issueBody := body
	// JavaScript arrays are objects, so an array is a valid empty issue patch.
	if bodyIsArray && r.Method == http.MethodPatch {
		issueBody = map[string]any{}
	}
	if a.servePullRoutes(w, r, parts, issueBody) {
		return
	}
	if a.serveIssueRoutes(w, r, parts, issueBody) {
		return
	}
	if a.serveContentRoutes(w, r, parts, body) {
		return
	}
	if a.serveRepoRoutes(w, r, parts, body) {
		return
	}
	sendJSON(w, 404, map[string]string{"message": "Not Found"})
}
