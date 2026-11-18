package main

import (
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func resolvePullRef(repo *Repo, ref string) string {
	sha, exists := repo.getRef("refs/heads/" + ref)
	if !exists && len(ref) == 40 {
		if _, err := hex.DecodeString(ref); err == nil {
			sha = strings.ToLower(ref)
		}
	}
	if kind, _, ok := repo.readObject(sha); ok && kind == "commit" {
		return sha
	}
	return ""
}

// Ancestors are visited breadth first, preserving commit parent order.
func pullAncestors(repo *Repo, start string) []string {
	queue := []string{start}
	seen := make(map[string]bool)
	result := []string{}
	for i := 0; i < len(queue); i++ {
		sha := queue[i]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		kind, data, ok := repo.readObject(sha)
		if !ok || kind != "commit" {
			continue
		}
		result = append(result, sha)
		queue = append(queue, parseCommit(data).Parents...)
	}
	return result
}

func pullMergeBase(repo *Repo, a, b string) string {
	common := make(map[string]bool)
	for _, sha := range pullAncestors(repo, a) {
		common[sha] = true
	}
	for _, sha := range pullAncestors(repo, b) {
		if common[sha] {
			return sha
		}
	}
	return ""
}

func pullTree(repo *Repo, sha string) map[string]FileEntry {
	tree := make(map[string]FileEntry)
	if kind, _, ok := repo.readObject(sha); !ok || kind != "commit" {
		return tree
	}
	for _, file := range repo.flattenTree(sha) {
		tree[file.Path] = file
	}
	return tree
}

type pullChange struct{ path, status, baseSHA, headSHA string }

// Mode-only changes are deliberately ignored, as in diffTrees in rest-pulls.ts.
func pullDiff(base, head map[string]FileEntry) []pullChange {
	paths := make(map[string]bool)
	for path := range base {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	changes := []pullChange{}
	for _, path := range names {
		b, h := base[path].SHA, head[path].SHA
		if b == h {
			continue
		}
		status := "modified"
		if b == "" {
			status = "added"
		} else if h == "" {
			status = "removed"
		}
		changes = append(changes, pullChange{path, status, b, h})
	}
	return changes
}

func pullLines(repo *Repo, sha string) []string {
	_, data, _ := repo.readObject(sha)
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.ToValidUTF8(string(data), "\ufffd"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type pullFile struct {
	Filename    string  `json:"filename"`
	Status      string  `json:"status"`
	SHA         *string `json:"sha"`
	Additions   int     `json:"additions"`
	Deletions   int     `json:"deletions"`
	Changes     int     `json:"changes"`
	BlobURL     string  `json:"blob_url"`
	RawURL      string  `json:"raw_url"`
	ContentsURL string  `json:"contents_url"`
}

func pullFiles(repo *Repo, pull *PullRequest) []pullFile {
	head, base := resolvePullRef(repo, pull.HeadRef), resolvePullRef(repo, pull.BaseRef)
	mb := ""
	if head != "" && base != "" {
		mb = pullMergeBase(repo, head, base)
	}
	files := []pullFile{}
	for _, change := range pullDiff(pullTree(repo, mb), pullTree(repo, head)) {
		b, h := pullLines(repo, change.baseSHA), pullLines(repo, change.headSHA)
		row := make([]int, len(h)+1)
		for _, line := range b {
			diagonal := 0
			for j := 1; j <= len(h); j++ {
				previous := row[j]
				if line == h[j-1] {
					row[j] = diagonal + 1
				} else if row[j-1] > row[j] {
					row[j] = row[j-1]
				}
				diagonal = previous
			}
		}
		f := pullFile{Filename: change.path, Status: change.status, Additions: len(h) - row[len(h)], Deletions: len(b) - row[len(h)]}
		if change.headSHA != "" {
			sha := change.headSHA
			f.SHA = &sha
		}
		f.Changes = f.Additions + f.Deletions
		files = append(files, f)
	}
	return files
}

func pullJSON(repo *Repo, pull *PullRequest) map[string]any {
	files := pullFiles(repo, pull)
	additions, deletions := 0, 0
	for _, file := range files {
		additions += file.Additions
		deletions += file.Deletions
	}
	refJSON := func(ref string) map[string]any {
		var sha any
		if resolved := resolvePullRef(repo, ref); resolved != "" {
			sha = resolved
		}
		return map[string]any{"ref": ref, "sha": sha, "label": ViewerLogin + ":" + ref}
	}
	var closedAt *string
	if pull.State == "closed" {
		closedAt = pull.MergedAt
	}
	return map[string]any{
		"id": pull.Number, "number": pull.Number, "title": pull.Title, "body": pull.Body,
		"state": pull.State, "merged": pull.Merged, "merged_at": pull.MergedAt, "merge_commit_sha": pull.MergeCommitSHA,
		"draft": false, "user": issueViewer, "head": refJSON(pull.HeadRef), "base": refJSON(pull.BaseRef),
		"created_at": pull.CreatedAt, "updated_at": pull.UpdatedAt, "closed_at": closedAt,
		"html_url": "", "url": "", "issue_url": "", "changed_files": len(files), "additions": additions, "deletions": deletions,
	}
}

func pullMergeable(repo *Repo, pull *PullRequest) bool {
	base, head := resolvePullRef(repo, pull.BaseRef), resolvePullRef(repo, pull.HeadRef)
	if base == "" || head == "" {
		return false
	}
	common := pullTree(repo, pullMergeBase(repo, base, head))
	baseChanges := make(map[string]string)
	for _, change := range pullDiff(common, pullTree(repo, base)) {
		baseChanges[change.path] = change.headSHA
	}
	for _, change := range pullDiff(common, pullTree(repo, head)) {
		if sha, exists := baseChanges[change.path]; exists && sha != change.headSHA {
			return false
		}
	}
	return true
}

// JavaScript truthiness and string conversion apply to merge message options.
func pullOption(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, v != ""
	case bool:
		return strconv.FormatBool(v), v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), v != 0
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i], _ = pullOption(item)
		}
		return strings.Join(parts, ","), true
	default:
		return "[object Object]", true
	}
}

func mergePull(repo *Repo, pull *PullRequest, body map[string]any) string {
	baseTip, headTip := resolvePullRef(repo, pull.BaseRef), resolvePullRef(repo, pull.HeadRef)
	base, head := pullTree(repo, baseTip), pullTree(repo, headTip)
	for _, change := range pullDiff(pullTree(repo, pullMergeBase(repo, baseTip, headTip)), head) {
		if change.headSHA == "" {
			delete(base, change.path)
		} else {
			base[change.path] = head[change.path]
		}
	}
	files := make([]FileEntry, 0, len(base))
	for _, file := range base {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	now := time.Now().UTC()
	title, ok := pullOption(body["commit_title"])
	if !ok {
		title = pull.Title
	}
	message := title
	if text, ok := pullOption(body["commit_message"]); ok {
		message += "\n\n" + text
	}
	parents := []string{baseTip}
	if body["merge_method"] == "merge" {
		parents = append(parents, headTip)
	}
	identity := commitIdentity(now)
	sha := repo.writeCommit(Commit{Tree: buildContentTree(&repo.GitStore, files), Parents: parents, Author: identity, Committer: identity, Message: message})
	repo.setRef("refs/heads/"+pull.BaseRef, sha)
	stamp := now.Format("2006-01-02T15:04:05.000Z")
	pull.State, pull.Merged, pull.UpdatedAt = "closed", true, stamp
	pull.MergedAt, pull.MergeCommitSHA = &stamp, &sha
	return sha
}

func (a *App) servePullRoutes(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) bool {
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v3" || parts[2] != "repos" || parts[5] != "pulls" {
		return false
	}
	tail := parts[6:]
	valid := len(tail) == 0 && (r.Method == "GET" || r.Method == "POST") || len(tail) == 1 && (r.Method == "GET" || r.Method == "PATCH") || len(tail) == 2 && (tail[1] == "files" && r.Method == "GET" || tail[1] == "merge" && r.Method == "PUT")
	if !valid {
		return false
	}
	owner, ownerErr := url.PathUnescape(parts[3])
	name, nameErr := url.PathUnescape(parts[4])
	if r.Method == "GET" {
		a.store.Mu.RLock()
		defer a.store.Mu.RUnlock()
	} else {
		a.store.Mu.Lock()
		defer a.store.Mu.Unlock()
	}
	repo := a.store.Repos[repoKey(owner, name)]
	if ownerErr != nil || nameErr != nil || owner != ViewerLogin || repo == nil {
		notFound(w)
		return true
	}
	if len(tail) == 0 {
		if r.Method == "GET" {
			listPulls(w, r, repo)
			return true
		}
		title, titleOK := body["title"].(string)
		head, headOK := body["head"].(string)
		base, baseOK := body["base"].(string)
		text, textOK := body["body"].(string)
		_, hasBody := body["body"]
		if !titleOK || strings.TrimSpace(title) == "" || !headOK || !baseOK || resolvePullRef(repo, head) == "" || resolvePullRef(repo, base) == "" || hasBody && !textOK {
			validationFailed(w)
			return true
		}
		repo.NumberSeq++
		now := issueNow()
		pull := &PullRequest{Number: repo.NumberSeq, Kind: "pull", Title: title, Body: text, State: "open", HeadRef: head, BaseRef: base, CreatedAt: now, UpdatedAt: now, Seq: repo.NumberSeq}
		repo.Items[pull.Number] = pull
		sendJSON(w, 201, pullJSON(repo, pull))
		return true
	}
	numberText, err := url.PathUnescape(tail[0])
	number, numberErr := strconv.Atoi(numberText)
	pull, _ := repo.Items[number].(*PullRequest)
	if err != nil || numberErr != nil || number <= 0 || strconv.Itoa(number) != numberText || pull == nil || pull.Kind != "pull" {
		notFound(w)
		return true
	}
	if len(tail) == 2 {
		if tail[1] == "files" {
			sendJSON(w, 200, pullFiles(repo, pull))
			return true
		}
		if pull.Merged || pull.State == "closed" {
			sendJSON(w, 405, map[string]string{"message": "Pull request already merged"})
			return true
		}
		if !pullMergeable(repo, pull) {
			sendJSON(w, 405, map[string]string{"message": "Pull Request is not mergeable"})
			return true
		}
		sha := mergePull(repo, pull, body)
		sendJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": "Pull request successfully merged and closed."})
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
				if !ok || key == "title" && strings.TrimSpace(text) == "" || key == "state" && text != "open" && text != "closed" {
					validationFailed(w)
					return true
				}
			}
		}
		if title, ok := body["title"].(string); ok {
			pull.Title = title
		}
		if text, ok := body["body"].(string); ok {
			pull.Body = text
		}
		if state, ok := body["state"].(string); ok {
			pull.State = state
		}
		pull.UpdatedAt = issueNow()
	}
	sendJSON(w, 200, pullJSON(repo, pull))
	return true
}

func listPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	positive := func(value string, fallback float64) float64 {
		n := issueQueryNumber(value, fallback)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fallback
		}
		return math.Max(1, math.Floor(n))
	}
	size, page := math.Min(100, positive(query.Get("per_page"), 30)), positive(query.Get("page"), 1)
	pulls := []*PullRequest{}
	for _, item := range repo.Items {
		if pull, ok := item.(*PullRequest); ok && pull.Kind == "pull" && (state == "all" || pull.State == state) {
			pulls = append(pulls, pull)
		}
	}
	sort.Slice(pulls, func(i, j int) bool {
		return pulls[i].CreatedAt > pulls[j].CreatedAt || pulls[i].CreatedAt == pulls[j].CreatedAt && pulls[i].Seq > pulls[j].Seq
	})
	out := []map[string]any{}
	start, end := math.Min(float64(len(pulls)), (page-1)*size), math.Min(float64(len(pulls)), page*size)
	for i := int(start); i < int(end); i++ {
		out = append(out, pullJSON(repo, pulls[i]))
	}
	sendJSON(w, 200, out)
}
