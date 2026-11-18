package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ViewerLogin   = "adaiversen"
	ViewerID      = 1000
	ViewerName    = "ada iversen"
	ViewerEmail   = "adaiversen@example.invalid"
	StrangerLogin = "olgabauer"
	StrangerID    = 1002
)

type Label struct {
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
}

type Issue struct {
	responseJSON atomic.Pointer[[]byte]
	Number       int
	Kind         string
	Title        string
	Body         string
	State        string
	Labels       []string
	CreatedAt    string
	UpdatedAt    string
	ClosedAt     *string
	Seq          int
	Comments     []Comment
}

type PullRequest struct {
	Number         int
	Kind           string
	Title          string
	Body           string
	State          string
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	CreatedAt      string
	UpdatedAt      string
	Seq            int
	Comments       []Comment
}

// Item represents either an issue or a pull request in the shared number space.
type Item interface{ repoItem() }

func (*Issue) repoItem()       {}
func (*PullRequest) repoItem() {}

type Repo struct {
	gitCacheMu sync.Mutex
	gitCache   map[string]*gitPackEntry
	GitStore
	ID            int
	Owner         string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Refs          map[string]string
	Head          string
	Labels        map[string]*Label
	LabelOrder    []string
	Items         map[int]Item
	NumberSeq     int
}

// Mu guards all store and repository state. Callers must hold it when using
// repository pointers returned by the helpers below.
type Store struct {
	Mu    sync.RWMutex
	Repos map[string]*Repo
	Seq   int
}

func NewStore() *Store {
	return &Store{Repos: make(map[string]*Repo), Seq: 1}
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner + "/" + name)
}

func (s *Store) NextID() int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.nextIDLocked()
}

func (s *Store) nextIDLocked() int {
	if s.Seq == 0 {
		s.Seq = 1
	}
	id := s.Seq
	s.Seq++
	return id
}

func (s *Store) CreateRepo(owner, name, description string, private bool, defaultBranch string) (*Repo, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if name == "" {
		return nil, errors.New("Validation Failed")
	}
	key := repoKey(owner, name)
	if _, exists := s.Repos[key]; exists {
		return nil, errors.New("name already exists on this account")
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	repo := &Repo{
		ID: s.nextIDLocked(), Owner: owner, Name: name, FullName: owner + "/" + name,
		Description: description, Private: private, DefaultBranch: defaultBranch,
		CreatedAt: now, UpdatedAt: now, Refs: make(map[string]string), Head: defaultBranch,
		Labels: make(map[string]*Label), Items: make(map[int]Item),
	}
	if s.Repos == nil {
		s.Repos = make(map[string]*Repo)
	}
	s.Repos[key] = repo
	return repo, nil
}

func (s *Store) GetRepo(owner, name string) (*Repo, bool) {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	repo, ok := s.Repos[repoKey(owner, name)]
	return repo, ok
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}
