package main

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func repoJSON(repo *Repo, r *http.Request) map[string]any {
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	base := "http://" + host + "/" + ViewerLogin + "/" + repo.Name
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return map[string]any{
		"id": repo.ID, "name": repo.Name, "full_name": ViewerLogin + "/" + repo.Name,
		"owner":   map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"private": repo.Private, "description": repo.Description, "default_branch": repo.DefaultBranch,
		"created_at": repo.CreatedAt, "updated_at": repo.UpdatedAt, "html_url": base,
		"clone_url": base + ".git", "git_url": "git://" + host + "/" + ViewerLogin + "/" + repo.Name + ".git",
		"ssh_url": "git@" + host + ":" + ViewerLogin + "/" + repo.Name + ".git",
		"size":    0, "stargazers_count": 0, "watchers_count": 0, "forks_count": 0, "open_issues_count": 0,
		"language": nil, "archived": false, "disabled": false, "visibility": visibility,
	}
}

func notFound(w http.ResponseWriter) { sendJSON(w, 404, map[string]string{"message": "Not Found"}) }
func validationFailed(w http.ResponseWriter) {
	sendJSON(w, 422, map[string]string{"message": "Validation Failed"})
}

// serveRepoRoutes holds the store lock through serialization so repository state
// and the returned representation always describe the same operation.
func (a *App) serveRepoRoutes(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) bool {
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "v3" {
		return false
	}
	if len(parts) == 4 && parts[2] == "user" && parts[3] == "repos" {
		switch r.Method {
		case http.MethodPost:
			name, ok := body["name"].(string)
			if !ok || name == "" {
				validationFailed(w)
				return true
			}
			description, _ := body["description"].(string)
			private, _ := body["private"].(bool)
			branch, _ := body["default_branch"].(string)
			repo, err := a.store.CreateRepo(ViewerLogin, name, description, private, branch)
			if err != nil {
				sendJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]string{"resource": "Repository", "field": "name", "code": "custom", "message": "name already exists on this account"}}})
				return true
			}
			a.store.Mu.RLock()
			defer a.store.Mu.RUnlock()
			sendJSON(w, 201, repoJSON(repo, r))
			return true
		case http.MethodGet:
			a.store.Mu.RLock()
			defer a.store.Mu.RUnlock()
			a.listRepos(w, r)
			return true
		}
		return false
	}
	if len(parts) < 5 || parts[2] != "repos" {
		return false
	}
	owner, err := url.PathUnescape(parts[3])
	if err != nil {
		return false
	}
	name, err := url.PathUnescape(parts[4])
	if err != nil {
		return false
	}
	a.store.Mu.Lock()
	defer a.store.Mu.Unlock()
	repo := a.store.Repos[repoKey(ViewerLogin, name)]
	if owner != ViewerLogin {
		repo = nil
	}
	tail := parts[5:]
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet, http.MethodPatch, http.MethodDelete:
			if repo == nil {
				notFound(w)
				return true
			}
		default:
			return false
		}
		switch r.Method {
		case http.MethodPatch:
			if value, ok := body["name"].(string); ok && value != repo.Name {
				delete(a.store.Repos, repoKey(ViewerLogin, repo.Name))
				repo.Name = value
				repo.FullName = ViewerLogin + "/" + value
				a.store.Repos[repoKey(ViewerLogin, value)] = repo
			}
			if value, ok := body["description"].(string); ok {
				repo.Description = value
			}
			if value, ok := body["private"].(bool); ok {
				repo.Private = value
			}
			if value, ok := body["default_branch"].(string); ok {
				repo.DefaultBranch = value
				repo.Head = value
			}
			repo.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		case http.MethodDelete:
			delete(a.store.Repos, repoKey(ViewerLogin, repo.Name))
			w.WriteHeader(204)
			return true
		}
		sendJSON(w, 200, repoJSON(repo, r))
		return true
	}
	if tail[0] == "branches" && r.Method == http.MethodGet {
		if repo == nil {
			notFound(w)
			return true
		}
		branch, _ := url.PathUnescape(strings.Join(tail[1:], "/"))
		out := make([]any, 0)
		for _, ref := range sortedRefs(repo) {
			if !strings.HasPrefix(ref, "refs/heads/") {
				continue
			}
			name := strings.TrimPrefix(ref, "refs/heads/")
			value := map[string]any{"name": name, "commit": map[string]string{"sha": repo.Refs[ref], "url": ""}, "protected": false}
			if len(tail) > 1 && name == branch {
				sendJSON(w, 200, value)
				return true
			}
			out = append(out, value)
		}
		if len(tail) > 1 {
			notFound(w)
		} else {
			sendJSON(w, 200, out)
		}
		return true
	}
	if len(tail) >= 2 && tail[0] == "git" && tail[1] == "refs" {
		return serveRefs(w, r, repo, tail[2:], body)
	}
	return false
}

func sortedRefs(repo *Repo) []string {
	refs := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func (a *App) listRepos(w http.ResponseWriter, r *http.Request) {
	number := func(key string, fallback float64) float64 {
		value := r.URL.Query().Get(key)
		if value == "" {
			return fallback
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if strings.TrimSpace(value) == "" {
			return 0
		}
		if err != nil {
			return math.NaN()
		}
		return n
	}
	n := math.Min(100, math.Max(1, number("per_page", 30)))
	p := math.Max(1, number("page", 1))
	repos := make([]*Repo, 0, len(a.store.Repos))
	for _, repo := range a.store.Repos {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	out := make([]any, 0)
	start, end := (p-1)*n, p*n
	// Array.slice truncates fractional indices and treats NaN as zero.
	index := func(v float64) int {
		if math.IsNaN(v) || v <= 0 {
			return 0
		}
		if v >= float64(len(repos)) {
			return len(repos)
		}
		return int(v)
	}
	for i, stop := index(start), index(end); i < stop; i++ {
		out = append(out, repoJSON(repos[i], r))
	}
	pages := math.Ceil(float64(len(repos)) / n)
	link := func(page float64, rel string) string {
		return fmt.Sprintf("<http://%s/api/v3/user/repos?per_page=%s&page=%s>; rel=\"%s\"", r.Host, strconv.FormatFloat(n, 'f', -1, 64), strconv.FormatFloat(page, 'f', -1, 64), rel)
	}
	links := []string{}
	if pages > p {
		links = append(links, link(p+1, "next"), link(pages, "last"))
	}
	if p > 1 {
		links = append(links, link(1, "first"), link(p-1, "prev"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	sendJSON(w, 200, out)
}

func refJSON(ref, sha string, full bool) map[string]any {
	value := map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "commit"}}
	if full {
		value["node_id"] = sha
		value["url"] = ""
	}
	return value
}

func serveRefs(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string, body map[string]any) bool {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if repo == nil {
		notFound(w)
		return true
	}
	ref, err := url.PathUnescape(strings.Join(tail, "/"))
	if err != nil {
		notFound(w)
		return true
	}
	ref = "refs/" + ref
	switch r.Method {
	case http.MethodGet:
		if len(tail) == 0 || ref == "refs/heads" {
			out := make([]any, 0)
			for _, key := range sortedRefs(repo) {
				if strings.HasPrefix(key, "refs/") && (len(tail) == 0 || strings.HasPrefix(key, "refs/heads/")) {
					out = append(out, refJSON(key, repo.Refs[key], false))
				}
			}
			sendJSON(w, 200, out)
		} else if sha, ok := repo.getRef(ref); ok {
			sendJSON(w, 200, refJSON(ref, sha, false))
		} else {
			notFound(w)
		}
	case http.MethodPost:
		if len(tail) != 0 {
			return false
		}
		value, ok := body["ref"].(string)
		sha, valid := body["sha"].(string)
		kind, _, exists := repo.readObject(sha)
		if !ok || !strings.HasPrefix(value, "refs/") || !valid || !exists || kind != "commit" {
			validationFailed(w)
			return true
		}
		repo.setRef(value, sha)
		sendJSON(w, 201, refJSON(value, sha, true))
	case http.MethodPatch:
		if len(tail) == 0 {
			return false
		}
		if _, ok := repo.getRef(ref); !ok {
			notFound(w)
			return true
		}
		sha, valid := body["sha"].(string)
		kind, _, exists := repo.readObject(sha)
		if !valid || !exists || kind != "commit" {
			validationFailed(w)
			return true
		}
		repo.setRef(ref, sha)
		sendJSON(w, 200, refJSON(ref, sha, true))
	case http.MethodDelete:
		if len(tail) == 0 {
			return false
		}
		if !repo.deleteRef(ref) {
			notFound(w)
			return true
		}
		w.WriteHeader(204)
	}
	return true
}
