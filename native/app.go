package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
	if strings.HasPrefix(path, "/api/v3/repos/") && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		segments := strings.Split(path, "/")
		getIssue := r.Method == http.MethodGet && len(segments) == 8 && segments[7] != "" && segments[7] != "labels"
		createIssue := r.Method == http.MethodPost && len(segments) == 7
		// Leave empty segments to the generic router, which discards them.
		if (getIssue || createIssue) && segments[4] != "" && segments[5] != "" && segments[6] == "issues" {
			a.serveIssueFast(w, segments, body, getIssue)
			return
		}
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
	issueParts := parts
	// Reuse the repository label list for GET /issues/labels.
	if r.Method == http.MethodGet && len(parts) == 7 && parts[5] == "issues" && parts[6] == "labels" {
		issueParts = []string{parts[0], parts[1], parts[2], parts[3], parts[4], "labels"}
	}
	if a.serveIssueRoutes(w, r, issueParts, issueBody) {
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

func (a *App) serveIssueFast(w http.ResponseWriter, segments []string, body map[string]any, getIssue bool) {
	owner, err := url.PathUnescape(segments[4])
	if err != nil {
		notFound(w)
		return
	}
	name, err := url.PathUnescape(segments[5])
	if err != nil {
		notFound(w)
		return
	}
	if getIssue {
		a.store.Mu.RLock()
		defer a.store.Mu.RUnlock()
	} else {
		a.store.Mu.Lock()
		defer a.store.Mu.Unlock()
	}
	repo := a.store.Repos[repoKey(owner, name)]
	if owner != ViewerLogin || repo == nil {
		notFound(w)
		return
	}
	if getIssue {
		numberText, _ := url.PathUnescape(segments[7])
		number, err := strconv.Atoi(numberText)
		item, _ := repo.Items[number].(*Issue)
		if err != nil || number <= 0 || strconv.Itoa(number) != numberText || item == nil || item.Kind != "issue" {
			notFound(w)
			return
		}
		sendIssueJSON(w, 200, item, repo)
		return
	}
	title, ok := body["title"].(string)
	text, textOK := body["body"].(string)
	_, hasBody := body["body"]
	labels := []string{}
	valid := true
	if value, exists := body["labels"]; exists {
		labels, valid = issueLabels(value, repo)
	}
	if !ok || strings.TrimSpace(title) == "" || (hasBody && !textOK) || !valid {
		validationFailed(w)
		return
	}
	now := issueNow()
	repo.NumberSeq++
	i := &Issue{Number: repo.NumberSeq, Kind: "issue", Title: title, Body: text, State: "open", Labels: labels, CreatedAt: now, UpdatedAt: now, Seq: repo.NumberSeq}
	repo.Items[i.Number] = i
	sendIssueJSON(w, 201, i, repo)
}
