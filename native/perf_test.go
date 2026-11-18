package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkIssueGET(b *testing.B) {
	a := NewApp("test")
	repo, _ := a.store.CreateRepo(ViewerLogin, "perf", "", false, "main")
	repo.Items[1] = &Issue{Number: 1, Kind: "issue", Title: "title", Body: strings.Repeat("x", 1024), State: "open"}
	r := httptest.NewRequest("GET", "/api/v3/repos/"+ViewerLogin+"/perf/issues/1", nil)
	r.Header.Set("Authorization", "Bearer test")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkIssuePOST(b *testing.B) {
	a := NewApp("test")
	a.store.CreateRepo(ViewerLogin, "perf", "", false, "main")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("POST", "/api/v3/repos/"+ViewerLogin+"/perf/issues", strings.NewReader(`{"title":"title","body":"body"}`))
		r.Header.Set("Authorization", "Bearer test")
		a.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkPush(b *testing.B) {
	a := NewApp("test")
	repo := &Repo{Refs: make(map[string]string)}
	old := gitZero
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		source := &GitStore{}
		blob := source.writeBlob(bytes.Repeat([]byte{byte(i)}, 1024))
		tree := source.writeTree([]TreeEntry{{Mode: "100644", Name: "file", SHA: blob}})
		var parents []string
		if old != gitZero {
			parents = []string{old}
		}
		tip := source.writeCommit(Commit{Tree: tree, Parents: parents, Message: fmt.Sprint(i)})
		pack, _ := buildPack([]string{tip, tree, blob}, source.Objects)
		body := append(gitPkt(old+" "+tip+" refs/heads/main\x00report-status\n"), pktFlush()...)
		body = append(body, pack...)
		b.StartTimer()
		a.gitReceive(httptest.NewRecorder(), repo, body)
		old = tip
	}
}

func BenchmarkShallowClones(b *testing.B) {
	a := NewApp("test")
	repo := &Repo{}
	tip := ""
	for i := 0; i < 200; i++ {
		blob := repo.writeBlob([]byte(fmt.Sprint(i) + strings.Repeat("x", 1024)))
		tree := repo.writeTree([]TreeEntry{{Mode: "100644", Name: "file", SHA: blob}})
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		tip = repo.writeCommit(Commit{Tree: tree, Parents: parents})
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			a.gitFetchPack(repo, []string{tip}, map[string]bool{}, map[string]bool{}, 1)
		}
	})
}
