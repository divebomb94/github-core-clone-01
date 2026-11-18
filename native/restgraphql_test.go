package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGraphqlOperations(t *testing.T) {
	app := NewApp("secret")
	repo, err := app.store.CreateRepo(ViewerLogin, "example", "", false, "main")
	if err != nil {
		t.Fatal(err)
	}
	id := graphqlOpaque("Repository:1")
	execute := func(query string, variables graphqlObject) graphqlObject {
		t.Helper()
		result := app.graphqlExecute(graphqlObject{"query": query, "variables": variables})
		if result["errors"] != nil {
			t.Fatalf("%s: %v", query, result)
		}
		return result["data"].(map[string]any)
	}
	created := execute(`mutation Create($input: CreateIssueInput!) { createIssue(input:$input) { issue { id number title } } }`, graphqlObject{"input": graphqlObject{"repositoryId": id, "title": "First"}})
	issue := created["createIssue"].(map[string]any)["issue"].(map[string]any)
	execute(`mutation { createIssue(input:{repositoryId:"`+id+`", title:"Second"}) { issue { number } } }`, nil)
	execute(`{ addComment(input:{subjectId:"`+issue["id"].(string)+`",body:"hello"}) { commentEdge { node { id body } } } }`, nil)
	execute(`{ closeIssue(input:{issueId:"`+issue["id"].(string)+`"}) { issue { state comments {totalCount} } } }`, nil)
	if repo.Items[1].(*Issue).State != "closed" || len(repo.Items[1].(*Issue).Comments) != 1 {
		t.Fatal("mutations did not update item")
	}
	data := execute(`# comment
 query Named($owner: String!) { r:repository(owner:$owner,name:"example") { ... on Repository { issues(first:1,orderBy:{direction:ASC}) { nodes { title } edges { cursor } totalCount pageInfo {hasNextPage endCursor} } } } }`, graphqlObject{"owner": ViewerLogin})
	connection := data["r"].(map[string]any)["issues"].(map[string]any)
	if connection["totalCount"] != 2 || connection["pageInfo"].(map[string]any)["endCursor"] != "MA==" {
		t.Fatal(connection)
	}
	data = execute(`{ repository(owner:"`+ViewerLogin+`",name:"example") { issues(first:1,after:"MA==") { nodes {title} pageInfo {hasNextPage} } } }`, nil)
	connection = data["repository"].(map[string]any)["issues"].(map[string]any)
	if connection["nodes"].([]any)[0].(map[string]any)["title"] != "Second" || connection["pageInfo"].(map[string]any)["hasNextPage"] != false {
		t.Fatal(connection)
	}
	for query, message := range map[string]string{
		`{viewer {missing}}`:              `Cannot query field "missing"`,
		`{viewer{login}} {viewer{login}}`: "Unexpected trailing tokens",
		`subscription {viewer{login}}`:    "Unknown operation",
		`{viewer @}`:                      "Unexpected token at 8",
		`{viewer`:                         "Expected token",
	} {
		result := app.graphqlExecute(graphqlObject{"query": query})
		if result["data"] != nil || result["errors"].([]any)[0].(map[string]any)["message"] != message {
			t.Fatalf("%q: %v", query, result)
		}
	}
}

func TestGraphqlBatchRoute(t *testing.T) {
	app := NewApp("secret")
	request := httptest.NewRequest("POST", "/api/graphql", strings.NewReader(`[{"query":"{viewer{login}}"},{"query":42},{"query":"{__type(name:SearchType){enumValues{name}}}"}]`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	var results []graphqlObject
	if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(results) != 3 || results[0]["data"] == nil || results[1]["errors"] == nil || results[2]["data"] == nil {
		t.Fatal(response.Body.String())
	}
}

func TestGraphqlIssueMutationsInvalidateRESTCache(t *testing.T) {
	for _, mutation := range []string{"closeIssue", "addComment"} {
		t.Run(mutation, func(t *testing.T) {
			app := NewApp("secret")
			repo, err := app.store.CreateRepo(ViewerLogin, "example", "", false, "main")
			if err != nil {
				t.Fatal(err)
			}
			request := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer secret")
				w := httptest.NewRecorder()
				app.ServeHTTP(w, r)
				if w.Code >= 400 {
					t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
				}
				return w
			}
			path := "/api/v3/repos/" + ViewerLogin + "/example/issues"
			request("POST", path, `{"title":"Cached issue"}`)
			request("GET", path+"/1", "")
			issue := repo.Items[1].(*Issue)
			if issue.responseJSON.Load() == nil {
				t.Fatal("REST response was not cached")
			}
			id := graphqlOpaque("Issue:1:1")
			query := `mutation { closeIssue(input:{issueId:"` + id + `"}) { issue { state } } }`
			if mutation == "addComment" {
				query = `mutation { addComment(input:{subjectId:"` + id + `",body:"hello"}) { commentEdge { node { body } } } }`
			}
			body, err := json.Marshal(graphqlObject{"query": query})
			if err != nil {
				t.Fatal(err)
			}
			response := request("POST", "/api/graphql", string(body))
			var result graphqlObject
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["errors"] != nil {
				t.Fatalf("mutation failed: %s (%v)", response.Body.String(), err)
			}
			var rest issueResponse
			if err := json.Unmarshal(request("GET", path+"/1", "").Body.Bytes(), &rest); err != nil {
				t.Fatal(err)
			}
			if rest.State != issue.State || rest.UpdatedAt != issue.UpdatedAt || rest.Comments != len(issue.Comments) {
				t.Fatalf("stale REST response: %+v; stored state=%s comments=%d", rest, issue.State, len(issue.Comments))
			}
			if mutation == "closeIssue" && (rest.State != "closed" || rest.ClosedAt == nil || *rest.ClosedAt != *issue.ClosedAt) {
				t.Fatalf("REST issue was not closed: %+v", rest)
			}
		})
	}
}
