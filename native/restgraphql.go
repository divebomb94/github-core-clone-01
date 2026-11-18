package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

type graphqlObject = map[string]any
type graphqlFunction func(graphqlObject) any
type graphqlField struct {
	name, alias string
	args        graphqlObject
	fields      []graphqlField
}

type graphqlError string

func graphqlFail(message string) { panic(graphqlError(message)) }

// The lexer is anchored at each position, just like the sticky TS regexp.
var graphqlToken = regexp.MustCompile(`^([\s\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+|#[^\n]*|,|\.\.\.|"(?:\\[^\n\r]|[^"\\])*"|[_A-Za-z][_0-9A-Za-z]*|-?\d+(?:\.\d+)?|[!$():{}\[\]=])`)

type graphqlParser struct {
	tokens    []string
	index     int
	variables graphqlObject
}

func (p *graphqlParser) peek() string {
	if p.index == len(p.tokens) {
		return ""
	}
	return p.tokens[p.index]
}
func (p *graphqlParser) take(expected string) string {
	token := p.peek()
	if token == "" || expected != "" && token != expected {
		if expected == "" {
			expected = "token"
		}
		graphqlFail("Expected " + expected)
	}
	p.index++
	return token
}
func (p *graphqlParser) value() any {
	token := p.take("")
	switch token {
	case "$":
		return p.variables[p.take("")]
	case "{":
		result := graphqlObject{}
		for p.peek() != "}" {
			key := p.take("")
			p.take(":")
			result[key] = p.value()
		}
		p.take("}")
		return result
	case "[":
		result := []any{}
		for p.peek() != "]" {
			result = append(result, p.value())
		}
		p.take("]")
		return result
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if strings.HasPrefix(token, `"`) {
		var result string
		if err := json.Unmarshal([]byte(token), &result); err != nil {
			graphqlFail(err.Error())
		}
		return result
	}
	if token[0] == '-' || token[0] >= '0' && token[0] <= '9' {
		n, _ := strconv.ParseFloat(token, 64)
		return n
	}
	return token
}
func (p *graphqlParser) selection() []graphqlField {
	p.take("{")
	fields := []graphqlField{}
	for p.peek() != "}" {
		if p.peek() == "..." {
			p.take("")
			p.take("on")
			p.take("")
			fields = append(fields, p.selection()...)
			continue
		}
		name := p.take("")
		field := graphqlField{name: name, alias: name, args: graphqlObject{}}
		if p.peek() == ":" {
			p.take("")
			field.name = p.take("")
		}
		if p.peek() == "(" {
			p.take("")
			for p.peek() != ")" {
				key := p.take("")
				p.take(":")
				field.args[key] = p.value()
			}
			p.take(")")
		}
		if p.peek() == "{" {
			field.fields = p.selection()
		}
		fields = append(fields, field)
	}
	p.take("}")
	return fields
}
func graphqlParse(query string, variables graphqlObject) []graphqlField {
	p := graphqlParser{variables: variables}
	for pos := 0; pos < len(query); {
		token := graphqlToken.FindString(query[pos:])
		if token == "" {
			graphqlFail(fmt.Sprintf("Unexpected token at %d", len(utf16.Encode([]rune(query[:pos])))))
		}
		pos += len(token)
		if strings.Trim(token, " \t\n\r\v\f\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff") == "" || token[0] == '#' || token == "," {
			continue
		}
		p.tokens = append(p.tokens, token)
	}
	if p.peek() != "{" {
		operation := p.take("")
		if operation != "query" && operation != "mutation" {
			graphqlFail("Unknown operation")
		}
		if p.peek() != "(" && p.peek() != "{" {
			p.take("")
		}
		if p.peek() == "(" {
			depth := 0
			for {
				token := p.take("")
				if token == "(" {
					depth++
				}
				if token == ")" {
					depth--
				}
				if depth == 0 {
					break
				}
			}
		}
	}
	fields := p.selection()
	if p.index != len(p.tokens) {
		graphqlFail("Unexpected trailing tokens")
	}
	return fields
}

var graphqlSchema = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments"},
	"PullRequest": {"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "createdAt", "updatedAt", "author"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "isPrivate", "createdAt", "updatedAt"},
}

func graphqlOpaque(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
func graphqlDecode(value any) string {
	text, _ := value.(string)
	// Buffer.from accepts URL-safe base64, omitted padding and ignored characters.
	var clean strings.Builder
	for _, c := range text {
		if c == '=' {
			break
		}
		if c == '-' {
			c = '+'
		}
		if c == '_' {
			c = '/'
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' {
			clean.WriteRune(c)
		}
	}
	data, _ := base64.RawStdEncoding.DecodeString(clean.String())
	return strings.ToValidUTF8(string(data), "\ufffd")
}
func graphqlUser() graphqlObject {
	return graphqlObject{"login": ViewerLogin, "id": graphqlOpaque(fmt.Sprintf("User:%d", ViewerID))}
}
func graphqlNumber(value any) float64 {
	if value == nil {
		return 0
	}
	if b, ok := value.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	if n, ok := value.(float64); ok {
		return n
	}
	if n, ok := value.(int); ok {
		return float64(n)
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return 0
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return math.NaN()
	}
	return n
}
func graphqlSliceIndex(n float64, length int) int {
	if math.IsNaN(n) {
		return 0
	}
	if n < 0 {
		n = math.Max(float64(length)+math.Ceil(n), 0)
	} else {
		n = math.Min(math.Floor(n), float64(length))
	}
	return int(n)
}
func graphqlConnection(items []any, args graphqlObject) graphqlObject {
	start := float64(0)
	if _, truthy := pullOption(args["after"]); truthy {
		start = graphqlNumber(graphqlDecode(args["after"])) + 1
	}
	first := float64(100)
	if args["first"] != nil {
		first = graphqlNumber(args["first"])
	}
	lo, hi := graphqlSliceIndex(start, len(items)), graphqlSliceIndex(start+first, len(items))
	if hi < lo {
		hi = lo
	}
	nodes := items[lo:hi]
	edges := []any{}
	var end any
	for index, node := range nodes {
		cursor := graphqlOpaque(strconv.FormatFloat(start+float64(index), 'f', -1, 64))
		edges = append(edges, graphqlObject{"node": node, "cursor": cursor})
		end = cursor
	}
	return graphqlObject{"nodes": nodes, "edges": edges, "totalCount": len(items), "pageInfo": graphqlObject{"hasNextPage": start+float64(len(nodes)) < float64(len(items)), "endCursor": end}}
}
func graphqlItemView(repo *Repo, item Item) graphqlObject {
	var number int
	var title, body, state, created, updated, kind string
	var comments []Comment
	labels := []any{}
	switch v := item.(type) {
	case *Issue:
		number, title, body, state, created, updated, kind, comments = v.Number, v.Title, v.Body, v.State, v.CreatedAt, v.UpdatedAt, "Issue", v.Comments
		for _, name := range v.Labels {
			labels = append(labels, graphqlObject{"name": name})
		}
	case *PullRequest:
		number, title, body, state, created, updated, kind, comments = v.Number, v.Title, v.Body, v.State, v.CreatedAt, v.UpdatedAt, "PullRequest", v.Comments
		if v.Merged {
			state = "MERGED"
		}
	}
	result := graphqlObject{"id": graphqlOpaque(fmt.Sprintf("%s:%d:%d", kind, repo.ID, number)), "number": number, "title": title, "body": body, "state": strings.ToUpper(state), "createdAt": created, "updatedAt": updated, "author": graphqlUser(), "labels": graphqlObject{"nodes": labels}, "comments": graphqlObject{"totalCount": len(comments)}}
	if pull, ok := item.(*PullRequest); ok {
		result["headRefName"], result["baseRefName"], result["merged"] = pull.HeadRef, pull.BaseRef, pull.Merged
		result["mergeable"] = "CONFLICTING"
		if pullMergeable(repo, pull) {
			result["mergeable"] = "MERGEABLE"
		}
		result["files"] = graphqlFunction(func(args graphqlObject) any {
			files := []any{}
			for _, file := range pullFiles(repo, pull) {
				files = append(files, graphqlObject{"path": file.Filename})
			}
			return graphqlConnection(files, args)
		})
	}
	return result
}

// Repo item maps represent the TS insertion-ordered Map; numbers are allocated
// monotonically and items are never reinserted by these routes.
func graphqlItems(repo *Repo) []Item {
	numbers := make([]int, 0, len(repo.Items))
	for number := range repo.Items {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	items := make([]Item, 0, len(numbers))
	for _, number := range numbers {
		items = append(items, repo.Items[number])
	}
	return items
}
func graphqlNumberedItem(repo *Repo, value any) Item {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return nil
	}
	return repo.Items[int(number)]
}

func graphqlRepoView(repo *Repo) graphqlObject {
	var oid any
	if sha, ok := repo.getRef("refs/heads/" + repo.DefaultBranch); ok {
		oid = sha
	}
	result := graphqlObject{"id": graphqlOpaque(fmt.Sprintf("Repository:%d", repo.ID)), "databaseId": repo.ID, "name": repo.Name, "nameWithOwner": repo.FullName, "owner": graphqlUser(), "defaultBranchRef": graphqlObject{"name": repo.DefaultBranch, "target": graphqlObject{"oid": oid}}, "viewerPermission": "ADMIN", "hasIssuesEnabled": true, "isPrivate": repo.Private, "createdAt": repo.CreatedAt, "updatedAt": repo.UpdatedAt}
	result["issue"] = graphqlFunction(func(args graphqlObject) any {
		if item, ok := graphqlNumberedItem(repo, args["number"]).(*Issue); ok {
			return graphqlItemView(repo, item)
		}
		return nil
	})
	result["pullRequest"] = graphqlFunction(func(args graphqlObject) any {
		if item, ok := graphqlNumberedItem(repo, args["number"]).(*PullRequest); ok {
			return graphqlItemView(repo, item)
		}
		return nil
	})
	result["issues"] = graphqlFunction(func(args graphqlObject) any {
		issues := []*Issue{}
		for _, item := range graphqlItems(repo) {
			issue, ok := item.(*Issue)
			if !ok {
				continue
			}
			include := args["states"] == nil
			switch states := args["states"].(type) {
			case []any:
				for _, state := range states {
					if state == strings.ToUpper(issue.State) {
						include = true
					}
				}
			case string:
				include = strings.Contains(states, strings.ToUpper(issue.State))
			}
			if include {
				issues = append(issues, issue)
			}
		}
		if order, ok := args["orderBy"].(map[string]any); ok {
			sort.SliceStable(issues, func(i, j int) bool {
				cmp := strings.Compare(issues[i].CreatedAt, issues[j].CreatedAt)
				if cmp == 0 {
					cmp = issues[i].Seq - issues[j].Seq
				}
				if order["direction"] == "DESC" {
					return cmp > 0
				}
				return cmp < 0
			})
		}
		nodes := []any{}
		for _, issue := range issues {
			nodes = append(nodes, graphqlItemView(repo, issue))
		}
		return graphqlConnection(nodes, args)
	})
	result["pullRequests"] = graphqlFunction(func(args graphqlObject) any {
		nodes := []any{}
		for _, item := range graphqlItems(repo) {
			if pull, ok := item.(*PullRequest); ok {
				nodes = append(nodes, graphqlItemView(repo, pull))
			}
		}
		return graphqlConnection(nodes, args)
	})
	return result
}
func (a *App) graphqlLocate(id any) (string, *Repo, Item) {
	parts := strings.Split(graphqlDecode(id), ":")
	var repoID, number float64 = math.NaN(), math.NaN()
	if len(parts) > 1 {
		repoID = graphqlNumber(parts[1])
	}
	if len(parts) > 2 {
		number = graphqlNumber(parts[2])
	}
	for _, repo := range a.store.Repos {
		if float64(repo.ID) == repoID {
			return parts[0], repo, repo.Items[int(number)]
		}
	}
	graphqlFail("Unknown node")
	return "", nil, nil
}
func (a *App) graphqlMutate(name string, args graphqlObject) any {
	input, _ := args["input"].(map[string]any)
	var id any
	for _, key := range []string{"repositoryId", "issueId", "pullRequestId", "subjectId"} {
		if input[key] != nil {
			id = input[key]
			break
		}
	}
	_, repo, item := a.graphqlLocate(id)
	now := issueNow()
	if name == "createIssue" || name == "createPullRequest" {
		title, ok := input["title"].(string)
		if !ok || strings.TrimSpace(title) == "" {
			graphqlFail("Title required")
		}
		repo.NumberSeq++
		number := repo.NumberSeq
		body, _ := input["body"].(string)
		var created Item
		key := "issue"
		if name == "createIssue" {
			created = &Issue{Number: number, Kind: "issue", Title: title, Body: body, State: "open", CreatedAt: now, UpdatedAt: now, Seq: number, Comments: []Comment{}, Labels: []string{}}
		} else {
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			h, _ := repo.getRef("refs/heads/" + head)
			b, _ := repo.getRef("refs/heads/" + base)
			if h == "" || b == "" {
				graphqlFail("Unknown branch")
			}
			created = &PullRequest{Number: number, Kind: "pull", Title: title, Body: body, State: "open", HeadRef: head, BaseRef: base, CreatedAt: now, UpdatedAt: now, Seq: number, Comments: []Comment{}}
			key = "pullRequest"
		}
		repo.Items[number] = created
		return graphqlObject{key: graphqlItemView(repo, created)}
	}
	if item == nil {
		graphqlFail("Unknown subject")
	}
	if issue, ok := item.(*Issue); ok && name == "closeIssue" {
		issue.State, issue.UpdatedAt, issue.ClosedAt = "closed", now, &now
		issue.responseJSON.Store(nil)
		return graphqlObject{"issue": graphqlItemView(repo, issue)}
	}
	if pull, ok := item.(*PullRequest); ok && name == "mergePullRequest" {
		method := input["mergeMethod"]
		if method == nil {
			method = "MERGE"
		}
		text, _ := pullOption(method)
		mergePull(repo, pull, graphqlObject{"merge_method": strings.ToLower(text)})
		return graphqlObject{"pullRequest": graphqlItemView(repo, pull)}
	}
	if name == "addComment" {
		body, ok := input["body"].(string)
		if !ok || strings.TrimSpace(body) == "" {
			graphqlFail("Comment body required")
		}
		// The route holds Store.Mu, so use the locked form of Store.NextID.
		comment := Comment{ID: a.store.nextIDLocked(), Body: body, CreatedAt: now, UpdatedAt: now}
		switch v := item.(type) {
		case *Issue:
			v.Comments = append(v.Comments, comment)
			v.UpdatedAt = now
			v.responseJSON.Store(nil)
		case *PullRequest:
			v.Comments = append(v.Comments, comment)
			v.UpdatedAt = now
		}
		return graphqlObject{"commentEdge": graphqlObject{"node": graphqlObject{"id": graphqlOpaque(fmt.Sprintf("Comment:%d", comment.ID)), "body": body}}}
	}
	graphqlFail("Invalid mutation " + name)
	return nil
}
func graphqlProject(object any, fields []graphqlField) any {
	if object == nil {
		return nil
	}
	if array, ok := object.([]any); ok {
		result := make([]any, len(array))
		for i, entry := range array {
			result[i] = graphqlProject(entry, fields)
		}
		return result
	}
	values, _ := object.(map[string]any)
	result := graphqlObject{}
	for _, field := range fields {
		value, ok := values[field.name]
		if !ok {
			graphqlFail(fmt.Sprintf("Cannot query field %q", field.name))
		}
		if function, ok := value.(graphqlFunction); ok {
			value = function(field.args)
		}
		if len(field.fields) > 0 {
			value = graphqlProject(value, field.fields)
		}
		result[field.alias] = value
	}
	return result
}
func (a *App) graphqlExecute(operation any) (result graphqlObject) {
	defer func() {
		if failure := recover(); failure != nil {
			if message, ok := failure.(graphqlError); ok {
				result = graphqlObject{"data": nil, "errors": []any{graphqlObject{"message": string(message)}}}
			} else {
				panic(failure)
			}
		}
	}()
	op, _ := operation.(map[string]any)
	query, ok := op["query"].(string)
	if !ok {
		graphqlFail("Expected query document")
	}
	variables, _ := op["variables"].(map[string]any)
	fields := graphqlParse(query, variables)
	root := graphqlObject{"viewer": graphqlUser()}
	root["repository"] = graphqlFunction(func(args graphqlObject) any {
		owner, _ := args["owner"].(string)
		name, _ := args["name"].(string)
		if repo := a.store.Repos[repoKey(owner, name)]; repo != nil {
			return graphqlRepoView(repo)
		}
		return nil
	})
	root["node"] = graphqlFunction(func(args graphqlObject) any {
		kind, repo, item := a.graphqlLocate(args["id"])
		if kind == "Repository" {
			return graphqlRepoView(repo)
		}
		if item != nil {
			return graphqlItemView(repo, item)
		}
		return nil
	})
	root["__type"] = graphqlFunction(func(args graphqlObject) any {
		name, _ := args["name"].(string)
		names, key := graphqlSchema[name], "fields"
		if name == "SearchType" {
			names, key = []string{"ISSUE", "REPOSITORY", "USER"}, "enumValues"
		}
		if names == nil {
			return nil
		}
		nodes := []any{}
		for _, name := range names {
			nodes = append(nodes, graphqlObject{"name": name})
		}
		return graphqlObject{key: nodes}
	})
	for _, name := range []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"} {
		root[name] = graphqlFunction(func(args graphqlObject) any { return a.graphqlMutate(name, args) })
	}
	return graphqlObject{"data": graphqlProject(root, fields)}
}
func (a *App) serveGraphqlRoutes(w http.ResponseWriter, r *http.Request, parts []string, data []byte) bool {
	if r.Method != http.MethodPost || len(parts) != 2 || parts[0] != "api" || parts[1] != "graphql" {
		return false
	}
	var body any
	_ = json.Unmarshal(data, &body)
	a.store.Mu.Lock()
	defer a.store.Mu.Unlock()
	if operations, ok := body.([]any); ok {
		results := make([]any, len(operations))
		for i, operation := range operations {
			results[i] = a.graphqlExecute(operation)
		}
		sendJSON(w, 200, results)
	} else {
		sendJSON(w, 200, a.graphqlExecute(body))
	}
	return true
}
