package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueEncodingAndCacheInvalidation(t *testing.T) {
	a := NewApp("test")
	repo, _ := a.store.CreateRepo(ViewerLogin, "issues", "", false, "main")
	repo.Labels["bug"] = &Label{Name: "bug", Color: "<red>", Description: "\u2028"}
	closed := "now"
	issue := &Issue{Number: 1, Kind: "issue", Title: "\"<&\n\xff", Body: "\u2029", State: "closed", Labels: []string{"bug"}, ClosedAt: &closed}
	repo.Items[1] = issue
	response := issueJSON(issue, repo)
	type plainIssueResponse issueResponse
	expected, _ := json.Marshal(plainIssueResponse(response))
	actual, _ := response.MarshalJSON()
	if !bytes.Equal(actual, expected) {
		t.Fatalf("encoding differs: %s != %s", actual, expected)
	}
	request := func(method, suffix, body string) []byte {
		r := httptest.NewRequest(method, "/api/v3/repos/"+ViewerLogin+"/issues/issues/1"+suffix, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code >= 400 {
			t.Fatalf("%s failed: %d %s", method, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	request("GET", "", "")
	request("PATCH", "", `{"title":"new","state":"open"}`)
	request("POST", "/comments", `{"body":"comment"}`)
	var result issueResponse
	if err := json.Unmarshal(request("GET", "", ""), &result); err != nil {
		t.Fatal(err)
	}
	if result.Title != "new" || result.ClosedAt != nil || result.Comments != 1 {
		t.Fatalf("stale response: %+v", result)
	}
}
