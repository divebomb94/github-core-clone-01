package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestGitShallowPackConcurrent(t *testing.T) {
	a := NewApp("test")
	repo := &Repo{}
	tips := []string{}
	for i := 0; i < 200; i++ {
		blob := repo.writeBlob([]byte(fmt.Sprintf("%04d%s", i, bytes.Repeat([]byte("x"), 1024))))
		tree := repo.writeTree([]TreeEntry{{Mode: "100644", Name: "file", SHA: blob}})
		var parents []string
		if i > 0 {
			parents = []string{tips[i-1]}
		}
		tips = append(tips, repo.writeCommit(Commit{Tree: tree, Parents: parents, Message: fmt.Sprint(i)}))
	}
	tip := tips[len(tips)-1]
	var wg sync.WaitGroup
	packs := make([][]byte, 50)
	for i := range packs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pack, lines := a.gitFetchPack(repo, []string{tip}, map[string]bool{}, map[string]bool{}, 1)
			packs[i] = pack
			if len(lines) != 1 || lines[0] != "shallow "+tip+"\n" {
				t.Errorf("unexpected shallow lines: %v", lines)
			}
		}(i)
	}
	wg.Wait()
	for _, pack := range packs {
		if &pack[0] != &packs[0][0] {
			t.Fatal("concurrent fetches did not share cached pack")
		}
	}
	received := &GitStore{}
	result, err := parsePack(packs[0], received)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 3 || received.Objects[tip].Type != "commit" {
		t.Fatalf("depth-1 pack should contain tip, tree, blob: %+v", result)
	}
	if _, ok := received.Objects[tips[198]]; ok {
		t.Fatal("shallow pack included parent")
	}
	if !gitIsAncestor(repo.Objects, tips[198], tip) || gitIsAncestor(repo.Objects, tip, tips[198]) {
		t.Fatal("incorrect ancestry")
	}
	delivered, lines := gitFetchSelection(repo.Objects, []string{tip}, map[string]bool{}, map[string]bool{tip: true}, 2)
	if len(delivered) != 6 || len(lines) != 2 {
		t.Fatalf("unexpected deepen result: %d objects, %v", len(delivered), lines)
	}
	delivered, _ = gitFetchSelection(repo.Objects, []string{tip}, map[string]bool{tip: true}, map[string]bool{}, 0)
	if len(delivered) != 0 {
		t.Fatal("known tip should deliver no objects")
	}
}

func TestGitPackCacheRepositoryIsolation(t *testing.T) {
	a := NewApp("test")
	repo := &Repo{}
	blob := repo.writeBlob([]byte("data"))
	tree := repo.writeTree([]TreeEntry{{Mode: "100644", Name: "file", SHA: blob}})
	tip := repo.writeCommit(Commit{Tree: tree})
	a.gitFetchPack(repo, []string{tip}, map[string]bool{}, map[string]bool{}, 1)
	defer func() {
		if recover() == nil {
			t.Fatal("cached pack exposed objects absent from the requested repository")
		}
	}()
	a.gitFetchPack(&Repo{}, []string{tip}, map[string]bool{}, map[string]bool{}, 1)
}
