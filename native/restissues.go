package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type issueUser struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

var issueViewer = issueUser{ViewerLogin, ViewerID, "User"}

type labelResponse struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	URL         string `json:"url"`
}

func labelJSON(l *Label) labelResponse {
	return labelResponse{Name: l.Name, Color: l.Color, Description: l.Description}
}

type issueResponse struct {
	ID                int             `json:"id"`
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	State             string          `json:"state"`
	Labels            []labelResponse `json:"labels"`
	User              issueUser       `json:"user"`
	AuthorAssociation string          `json:"author_association"`
	Comments          int             `json:"comments"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
	ClosedAt          *string         `json:"closed_at"`
	HTMLURL           string          `json:"html_url"`
	URL               string          `json:"url"`
	RepositoryURL     string          `json:"repository_url"`
}

func issueJSON(i *Issue, repo *Repo) issueResponse {
	labels := make([]labelResponse, 0, len(i.Labels))
	for _, name := range i.Labels {
		if l := repo.Labels[name]; l != nil {
			labels = append(labels, labelJSON(l))
		}
	}
	return issueResponse{ID: i.Number, Number: i.Number, Title: i.Title, Body: i.Body,
		State: i.State, Labels: labels, User: issueViewer, AuthorAssociation: "OWNER",
		Comments: len(i.Comments), CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, ClosedAt: i.ClosedAt}
}

// Issue bytes are immutable and shared by GETs until a mutation invalidates them.
// Store.Mu protects issue fields; the atomic also permits concurrent readers to
// populate a previously empty cache without upgrading the store lock.
func sendIssueJSON(w http.ResponseWriter, status int, i *Issue, repo *Repo) {
	data := i.responseJSON.Load()
	if data == nil {
		encoded, _ := issueJSON(i, repo).MarshalJSON()
		data = &encoded
		i.responseJSON.Store(data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(*data)
}

// Keep the wire field order and encoding/json's string escaping, but avoid
// reflecting over the response and repeatedly encoding the constant user.
func (i issueResponse) MarshalJSON() ([]byte, error) {
	out := make([]byte, 0, 384+len(i.Title)+len(i.Body))
	text := func(value string) {
		// Ordinary ASCII titles, bodies, and timestamps need no temporary string
		// encoding. Delegate escaping and malformed UTF-8 handling to encoding/json.
		for n := 0; n < len(value); n++ {
			c := value[n]
			if c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
				encoded, _ := json.Marshal(value)
				out = append(out, encoded...)
				return
			}
		}
		out = append(out, '"')
		out = append(out, value...)
		out = append(out, '"')
	}
	out = append(out, `{"id":`...)
	out = strconv.AppendInt(out, int64(i.ID), 10)
	out = append(out, `,"number":`...)
	out = strconv.AppendInt(out, int64(i.Number), 10)
	out = append(out, `,"title":`...)
	text(i.Title)
	out = append(out, `,"body":`...)
	text(i.Body)
	out = append(out, `,"state":`...)
	text(i.State)
	out = append(out, `,"labels":[`...)
	for n, label := range i.Labels {
		if n > 0 {
			out = append(out, ',')
		}
		encoded, _ := json.Marshal(label)
		out = append(out, encoded...)
	}
	out = append(out, `],"user":{"login":"`+ViewerLogin+`","id":1000,"type":"User"},"author_association":"OWNER","comments":`...)
	out = strconv.AppendInt(out, int64(i.Comments), 10)
	out = append(out, `,"created_at":`...)
	text(i.CreatedAt)
	out = append(out, `,"updated_at":`...)
	text(i.UpdatedAt)
	out = append(out, `,"closed_at":`...)
	if i.ClosedAt == nil {
		out = append(out, "null"...)
	} else {
		text(*i.ClosedAt)
	}
	out = append(out, `,"html_url":"","url":"","repository_url":""}`...)
	return out, nil
}

type commentResponse struct {
	ID        int       `json:"id"`
	Body      string    `json:"body"`
	User      issueUser `json:"user"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
	URL       string    `json:"url"`
}

func commentJSON(c Comment) commentResponse {
	return commentResponse{ID: c.ID, Body: c.Body, User: issueViewer, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func issueLabels(value any, repo *Repo) ([]string, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	labels := make([]string, len(values))
	for i, value := range values {
		name, ok := value.(string)
		if !ok || repo.Labels[name] == nil {
			return nil, false
		}
		labels[i] = name
	}
	return labels, true
}

func issueNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func (a *App) serveIssueRoutes(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) bool {
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v3" || parts[2] != "repos" {
		return false
	}
	tail := parts[5:]
	labelsRoute := tail[0] == "labels" && ((len(tail) == 1 && (r.Method == "GET" || r.Method == "POST")) || (len(tail) == 2 && r.Method == "GET"))
	issuesRoute := tail[0] == "issues" && ((len(tail) == 1 && (r.Method == "GET" || r.Method == "POST")) || (len(tail) == 2 && (r.Method == "GET" || r.Method == "PATCH")) || (len(tail) == 3 && tail[2] == "comments" && (r.Method == "GET" || r.Method == "POST")))
	if !labelsRoute && !issuesRoute {
		return false
	}
	owner, err := url.PathUnescape(parts[3])
	if err != nil {
		notFound(w)
		return true
	}
	name, err := url.PathUnescape(parts[4])
	if err != nil {
		notFound(w)
		return true
	}
	if r.Method == "GET" {
		a.store.Mu.RLock()
		defer a.store.Mu.RUnlock()
	} else {
		a.store.Mu.Lock()
		defer a.store.Mu.Unlock()
	}
	repo := a.store.Repos[repoKey(owner, name)]
	if owner != ViewerLogin || repo == nil {
		notFound(w)
		return true
	}
	if labelsRoute {
		if len(tail) == 2 {
			name, _ := url.PathUnescape(tail[1])
			if label := repo.Labels[name]; label != nil {
				sendJSON(w, 200, labelJSON(label))
			} else {
				notFound(w)
			}
		} else if r.Method == "GET" {
			out := make([]labelResponse, 0, len(repo.Labels))
			for _, name := range repo.LabelOrder {
				out = append(out, labelJSON(repo.Labels[name]))
			}
			sendJSON(w, 200, out)
		} else {
			name, ok := body["name"].(string)
			if !ok || name == "" || repo.Labels[name] != nil {
				validationFailed(w)
				return true
			}
			color, ok := body["color"].(string)
			if !ok {
				color = "ededed"
			}
			description, _ := body["description"].(string)
			label := &Label{Name: name, Color: strings.TrimPrefix(color, "#"), Description: description}
			repo.Labels[name] = label
			repo.LabelOrder = append(repo.LabelOrder, name)
			sendJSON(w, 201, labelJSON(label))
		}
		return true
	}
	if len(tail) == 1 {
		if r.Method == "GET" {
			listIssues(w, r, repo)
			return true
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
			return true
		}
		now := issueNow()
		repo.NumberSeq++
		i := &Issue{Number: repo.NumberSeq, Kind: "issue", Title: title, Body: text, State: "open", Labels: labels, CreatedAt: now, UpdatedAt: now, Seq: repo.NumberSeq}
		repo.Items[i.Number] = i
		sendIssueJSON(w, 201, i, repo)
		return true
	}
	numberText, _ := url.PathUnescape(tail[1])
	if len(tail) == 3 {
		a.serveIssueComments(w, r, repo, numberText, body)
		return true
	}
	number, err := strconv.Atoi(numberText)
	i, _ := repo.Items[number].(*Issue)
	if err != nil || number <= 0 || strconv.Itoa(number) != numberText || i == nil || i.Kind != "issue" {
		notFound(w)
		return true
	}
	if r.Method == "PATCH" {
		if body == nil {
			validationFailed(w)
			return true
		}
		for _, key := range []string{"title", "body", "state"} {
			if value, exists := body[key]; exists {
				text, ok := value.(string)
				if !ok || (key == "title" && strings.TrimSpace(text) == "") || (key == "state" && text != "open" && text != "closed") {
					validationFailed(w)
					return true
				}
			}
		}
		labels := i.Labels
		if value, exists := body["labels"]; exists {
			var ok bool
			labels, ok = issueLabels(value, repo)
			if !ok {
				validationFailed(w)
				return true
			}
		}
		now := issueNow()
		if value, ok := body["title"].(string); ok {
			i.Title = value
		}
		if value, ok := body["body"].(string); ok {
			i.Body = value
		}
		if state, ok := body["state"].(string); ok {
			if state == "closed" && i.State != "closed" {
				i.ClosedAt = &now
			}
			if state == "open" {
				i.ClosedAt = nil
			}
			i.State = state
		}
		i.Labels = labels
		i.UpdatedAt = now
		i.responseJSON.Store(nil)
	}
	sendIssueJSON(w, 200, i, repo)
	return true
}

func (a *App) serveIssueComments(w http.ResponseWriter, r *http.Request, repo *Repo, numberText string, body map[string]any) {
	// Comment routes use JavaScript Number coercion and accept both item kinds.
	number := issueQueryNumber(numberText, 0)
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		notFound(w)
		return
	}
	var comments *[]Comment
	var updatedAt *string
	switch item := repo.Items[int(number)].(type) {
	case *Issue:
		if r.Method == "POST" {
			item.responseJSON.Store(nil)
		}
		comments, updatedAt = &item.Comments, &item.UpdatedAt
	case *PullRequest:
		comments, updatedAt = &item.Comments, &item.UpdatedAt
	default:
		notFound(w)
		return
	}
	if r.Method == "POST" {
		text, ok := body["body"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			validationFailed(w)
			return
		}
		now := issueNow()
		comment := Comment{ID: a.store.nextIDLocked(), Body: text, CreatedAt: now, UpdatedAt: now}
		*comments = append(*comments, comment)
		*updatedAt = now
		sendJSON(w, 201, commentJSON(comment))
		return
	}
	out := make([]commentResponse, 0, len(*comments))
	for _, comment := range *comments {
		out = append(out, commentJSON(comment))
	}
	sendJSON(w, 200, out)
}

func issueQueryNumber(value string, fallback float64) float64 {
	if value == "" {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if len(value) > 2 && value[0] == '0' && strings.ContainsAny(value[1:2], "xXbBoO") {
		if n, err := strconv.ParseUint(value, 0, 64); err == nil {
			return float64(n)
		}
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return math.NaN()
	}
	return n
}

func listIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	issues := make([]*Issue, 0)
	for _, item := range repo.Items {
		if i, ok := item.(*Issue); ok && i.Kind == "issue" && (state == "all" || i.State == state) {
			issues = append(issues, i)
		}
	}
	sort.Slice(issues, func(a, b int) bool {
		i, j := issues[a], issues[b]
		if query.Get("direction") != "asc" {
			i, j = j, i
		}
		return i.CreatedAt < j.CreatedAt || (i.CreatedAt == j.CreatedAt && i.Seq < j.Seq)
	})
	n := math.Min(100, math.Max(1, issueQueryNumber(query.Get("per_page"), 30)))
	page := math.Max(1, issueQueryNumber(query.Get("page"), 1))
	index := func(v float64) int {
		if math.IsNaN(v) || v <= 0 {
			return 0
		}
		if v >= float64(len(issues)) {
			return len(issues)
		}
		return int(v)
	}
	out := make([]issueResponse, 0)
	for i, end := index((page-1)*n), index(page*n); i < end; i++ {
		out = append(out, issueJSON(issues[i], repo))
	}
	sendJSON(w, 200, out)
}
