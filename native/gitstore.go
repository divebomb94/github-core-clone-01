package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GitObject holds the uncompressed content, without Git's hashing header.
type GitObject struct {
	Type    string
	Content []byte
}

// GitStore belongs to one repository. Like Refs, it is guarded by Store.Mu.
// Its zero value is ready to use.
type GitStore struct {
	Objects map[string]GitObject
}

type TreeEntry struct {
	Mode string
	Name string
	SHA  string
}

type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// FileEntry slices preserve the depth-first order of the encoded trees.
type FileEntry struct {
	Path string
	Mode string
	SHA  string
}

func (s *GitStore) writeObject(kind string, content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(content))
	h.Write(content)
	sha := hex.EncodeToString(h.Sum(nil))
	if s.Objects == nil {
		s.Objects = make(map[string]GitObject)
	}
	s.Objects[sha] = GitObject{Type: kind, Content: bytes.Clone(content)}
	return sha
}

func (s *GitStore) writeBlob(content []byte) string {
	return s.writeObject("blob", content)
}

func (s *GitStore) readObject(sha string) (kind string, content []byte, ok bool) {
	o, ok := s.Objects[sha]
	return o.Type, bytes.Clone(o.Content), ok
}

func (s *GitStore) writeTree(entries []TreeEntry) string {
	sorted := append([]TreeEntry(nil), entries...)
	key := func(e TreeEntry) string {
		if e.Mode == "40000" {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return key(sorted[i]) < key(sorted[j])
	})
	var content bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&content, "%s %s\x00", e.Mode, e.Name)
		sha, _ := hex.DecodeString(e.SHA)
		content.Write(sha)
	}
	return s.writeObject("tree", content.Bytes())
}

func parseTree(content []byte) []TreeEntry {
	var entries []TreeEntry
	for len(content) > 0 {
		zero := bytes.IndexByte(content, 0)
		if zero < 0 || len(content) < zero+21 {
			break
		}
		space := bytes.IndexByte(content[:zero], ' ')
		if space < 0 {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(content[:space]),
			Name: string(content[space+1 : zero]),
			SHA:  hex.EncodeToString(content[zero+1 : zero+21]),
		})
		content = content[zero+21:]
	}
	return entries
}

// writeCommit preserves supplied identities and message bytes exactly, as in
// gitstore.ts. In particular, it does not append a newline to the message.
func (s *GitStore) writeCommit(c Commit) string {
	var content strings.Builder
	fmt.Fprintf(&content, "tree %s\n", c.Tree)
	for _, parent := range c.Parents {
		fmt.Fprintf(&content, "parent %s\n", parent)
	}
	fmt.Fprintf(&content, "author %s\ncommitter %s\n\n%s", c.Author, c.Committer, c.Message)
	return s.writeObject("commit", []byte(content.String()))
}

// commitIdentity matches the identity stamped by the TypeScript callers.
func commitIdentity(at time.Time) string {
	return fmt.Sprintf("%s <%s> %d +0000", ViewerName, ViewerEmail, at.Unix())
}

func parseCommit(content []byte) Commit {
	head, message, _ := strings.Cut(string(content), "\n\n")
	c := Commit{Message: message}
	for _, line := range strings.Split(head, "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author = value
		case "committer":
			c.Committer = value
		}
	}
	return c
}

// flattenTree accepts a commit SHA, or a tree SHA like the TypeScript helper.
// Missing objects yield an empty list; directory entries themselves are omitted.
func (s *GitStore) flattenTree(commitSHA string) []FileEntry {
	kind, content, ok := s.readObject(commitSHA)
	if !ok {
		return nil
	}
	if kind == "commit" {
		_, content, _ = s.readObject(parseCommit(content).Tree)
	} else if kind != "tree" {
		return nil
	}
	var files []FileEntry
	var walk func([]byte, string)
	walk = func(data []byte, prefix string) {
		for _, e := range parseTree(data) {
			path := prefix + e.Name
			if e.Mode == "40000" {
				_, child, _ := s.readObject(e.SHA)
				walk(child, path+"/")
			} else {
				files = append(files, FileEntry{Path: path, Mode: e.Mode, SHA: e.SHA})
			}
		}
	}
	walk(content, "")
	return files
}

func (s *GitStore) blobAtPath(commitSHA, path string) (string, bool) {
	for _, file := range s.flattenTree(commitSHA) {
		if file.Path == path {
			return file.SHA, true
		}
	}
	return "", false
}

func (r *Repo) getRef(ref string) (string, bool) {
	sha, ok := r.Refs[ref]
	return sha, ok
}

func (r *Repo) setRef(ref, sha string) {
	if r.Refs == nil {
		r.Refs = make(map[string]string)
	}
	r.Refs[ref] = sha
}

func (r *Repo) deleteRef(ref string) bool {
	_, ok := r.Refs[ref]
	delete(r.Refs, ref)
	return ok
}

func (r *Repo) headTarget() string {
	branch := r.Head
	if branch == "" {
		branch = r.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}
	return "refs/heads/" + branch
}

func (r *Repo) resolveHEAD() (string, bool) {
	return r.getRef(r.headTarget())
}
