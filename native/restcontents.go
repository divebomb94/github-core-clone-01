package main

import (
	"encoding/base64"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// buildContentTree follows buildTree's rule that a file wins over descendants
// with the same name. The input order is retained until writeTree sorts entries.
func buildContentTree(s *GitStore, files []FileEntry) string {
	groups := make(map[string][]FileEntry)
	names := []string{}
	for _, f := range files {
		head, tail, _ := strings.Cut(f.Path, "/")
		if _, ok := groups[head]; !ok {
			names = append(names, head)
		}
		f.Path = tail
		groups[head] = append(groups[head], f)
	}
	entries := []TreeEntry{}
	for _, name := range names {
		group := groups[name]
		var direct *FileEntry
		for i := range group {
			if group[i].Path == "" {
				direct = &group[i]
			}
		}
		if direct != nil {
			entries = append(entries, TreeEntry{Mode: direct.Mode, Name: name, SHA: direct.SHA})
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, SHA: buildContentTree(s, group)})
		}
	}
	return s.writeTree(entries)
}

var contentIdentityPattern = regexp.MustCompile(`^(.*?) <([^>]+)> (\d+) ([+-]\d+)$`)

func contentIdentity(value string) map[string]string {
	name, email := "", ""
	date := time.Unix(0, 0).UTC()
	if m := contentIdentityPattern.FindStringSubmatch(value); m != nil {
		name, email = m[1], m[2]
		seconds, _ := strconv.ParseInt(m[3], 10, 64)
		date = time.Unix(seconds, 0).UTC()
	}
	return map[string]string{"name": name, "email": email, "date": date.Format("2006-01-02T15:04:05.000Z")}
}

func contentCommitJSON(s *GitStore, sha string) map[string]any {
	kind, data, ok := s.readObject(sha)
	if !ok || kind != "commit" {
		return nil
	}
	c := parseCommit(data)
	parents := make([]any, 0, len(c.Parents))
	for _, parent := range c.Parents {
		parents = append(parents, map[string]string{"sha": parent})
	}
	return map[string]any{
		"sha": sha, "node_id": sha,
		"commit":  map[string]any{"author": contentIdentity(c.Author), "committer": contentIdentity(c.Committer), "message": c.Message, "tree": map[string]string{"sha": c.Tree}, "comment_count": 0},
		"parents": parents, "url": "", "html_url": "",
	}
}

func resolveContentRef(repo *Repo, ref string) string {
	if ref == "" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if kind, _, ok := repo.readObject(ref); ok && kind == "commit" {
		return ref
	}
	branch := ref
	if !strings.HasPrefix(branch, "refs/") {
		branch = "refs/heads/" + branch
	}
	if sha := repo.Refs[branch]; sha != "" {
		return sha
	}
	return repo.Refs["refs/tags/"+ref]
}

func contentFileJSON(repo *Repo, path, sha string, includeContent bool) map[string]any {
	_, data, _ := repo.readObject(sha)
	name := path[strings.LastIndex(path, "/")+1:]
	out := map[string]any{"name": name, "path": path, "sha": sha, "size": len(data), "type": "file"}
	if includeContent {
		out["encoding"] = "base64"
		out["content"] = base64.StdEncoding.EncodeToString(data)
	}
	return out
}

// serveContentRoutes owns its lock, including reads of the object store.
func (a *App) serveContentRoutes(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) bool {
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v3" || parts[2] != "repos" {
		return false
	}
	endpoint := parts[5]
	if endpoint != "contents" && endpoint != "commits" {
		return false
	}
	if endpoint == "commits" {
		if r.Method != http.MethodGet || len(parts) > 7 {
			return false
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
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
	path, err := url.PathUnescape(strings.Join(parts[6:], "/"))
	if err != nil {
		return false
	}
	a.store.Mu.Lock()
	defer a.store.Mu.Unlock()
	repo := a.store.Repos[repoKey(ViewerLogin, name)]
	if endpoint == "commits" && len(parts) == 7 {
		// The source's single-commit route reads the global object store without
		// checking repository ownership or membership.
		for _, candidate := range a.store.Repos {
			if value := contentCommitJSON(&candidate.GitStore, path); value != nil {
				sendJSON(w, 200, value)
				return true
			}
		}
		notFound(w)
		return true
	}
	if owner != ViewerLogin || repo == nil {
		notFound(w)
		return true
	}
	if endpoint == "commits" {
		serveContentCommits(w, r, repo)
		return true
	}
	if r.Method == http.MethodGet {
		serveContentGet(w, r, repo, path)
		return true
	}
	message, validMessage := body["message"].(string)
	if !validMessage {
		validationFailed(w)
		return true
	}
	branch, ok := body["branch"].(string)
	if !ok {
		branch = repo.DefaultBranch
	}
	tip := repo.Refs["refs/heads/"+branch]
	files := repo.flattenTree(tip)
	oldIndex := -1
	for i, f := range files {
		if f.Path == path {
			oldIndex = i
			break
		}
	}
	sha, validSHA := body["sha"].(string)
	var bytes []byte
	if r.Method == http.MethodPut {
		encoded, ok := body["content"].(string)
		if !ok {
			validationFailed(w)
			return true
		}
		// ECMAScript whitespace, as accepted by Buffer.from and removed by /\s/g.
		clean := strings.Map(func(c rune) rune {
			if c == 0xFEFF || c == 0xA0 || c == 0x1680 || c >= 0x2000 && c <= 0x200A || c == 0x2028 || c == 0x2029 || c == 0x202F || c == 0x205F || c == 0x3000 || c == ' ' || c >= '\t' && c <= '\r' {
				return -1
			}
			return c
		}, encoded)
		bytes, err = base64.StdEncoding.DecodeString(clean)
		if err != nil || base64.StdEncoding.EncodeToString(bytes) != clean {
			validationFailed(w)
			return true
		}
		if oldIndex >= 0 && !validSHA {
			validationFailed(w)
			return true
		}
		if oldIndex < 0 {
			if _, exists := body["sha"]; exists {
				validationFailed(w)
				return true
			}
		}
	} else {
		if !validSHA {
			validationFailed(w)
			return true
		}
		if tip == "" || oldIndex < 0 {
			notFound(w)
			return true
		}
	}
	if oldIndex >= 0 && files[oldIndex].SHA != sha {
		sendJSON(w, 409, map[string]string{"message": "Conflict"})
		return true
	}
	blob := ""
	if r.Method == http.MethodPut {
		blob = repo.writeBlob(bytes)
		file := FileEntry{Path: path, Mode: "100644", SHA: blob}
		if oldIndex < 0 {
			files = append(files, file)
		} else {
			files[oldIndex] = file
		}
	} else {
		files = append(files[:oldIndex], files[oldIndex+1:]...)
	}
	parents := []string{}
	if tip != "" {
		parents = append(parents, tip)
	}
	identity := commitIdentity(time.Now())
	commit := repo.writeCommit(Commit{Tree: buildContentTree(&repo.GitStore, files), Parents: parents, Author: identity, Committer: identity, Message: message})
	repo.setRef("refs/heads/"+branch, commit)
	var content any
	status := 200
	if r.Method == http.MethodPut {
		content = contentFileJSON(repo, path, blob, true)
		if oldIndex < 0 {
			status = 201
		}
	}
	sendJSON(w, status, map[string]any{"content": content, "commit": contentCommitJSON(&repo.GitStore, commit)})
	return true
}

func serveContentGet(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	sha := resolveContentRef(repo, r.URL.Query().Get("ref"))
	kind, _, ok := repo.readObject(sha)
	if !ok || kind != "commit" {
		notFound(w)
		return
	}
	files := repo.flattenTree(sha)
	for _, file := range files {
		if file.Path == path {
			sendJSON(w, 200, contentFileJSON(repo, path, file.SHA, true))
			return
		}
	}
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}
	names := map[string]map[string]any{}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(file.Path, prefix)
		name, _, directory := strings.Cut(rest, "/")
		if name == "" {
			continue
		}
		if directory {
			names[name] = map[string]any{"name": name, "path": prefix + name, "sha": file.SHA, "size": 0, "type": "dir"}
		} else {
			names[name] = contentFileJSON(repo, prefix+name, file.SHA, false)
		}
	}
	if len(names) == 0 {
		notFound(w)
		return
	}
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, name := range keys {
		out = append(out, names[name])
	}
	sendJSON(w, 200, out)
}

func serveContentCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	query := r.URL.Query()
	start := resolveContentRef(repo, query.Get("sha"))
	all := []any{}
	if start == "" && query.Get("sha") != "" {
		notFound(w)
		return
	}
	for sha := start; sha != ""; {
		value := contentCommitJSON(&repo.GitStore, sha)
		if value == nil {
			break
		}
		all = append(all, value)
		_, data, _ := repo.readObject(sha)
		parents := parseCommit(data).Parents
		sha = ""
		if len(parents) > 0 {
			sha = parents[0]
		}
	}
	number := func(key string, fallback float64) float64 {
		raw := query.Get(key)
		if raw == "" {
			return fallback
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return math.NaN()
		}
		return n
	}
	n := math.Min(100, math.Max(1, number("per_page", 30)))
	page := math.Max(1, number("page", 1))
	index := func(v float64) int {
		if math.IsNaN(v) || v <= 0 {
			return 0
		}
		if v >= float64(len(all)) {
			return len(all)
		}
		return int(v)
	}
	first, last := index((page-1)*n), index(page*n)
	if last < first {
		last = first
	}
	sendJSON(w, 200, all[first:last])
}
