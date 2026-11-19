package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const gitZero = "0000000000000000000000000000000000000000"

var gitOID = regexp.MustCompile(`^[0-9a-f]{40}$`)

type gitPackEntry struct {
	ready chan struct{}
	pack  []byte
	lines []string
	err   error
}
type gitRef struct{ name, oid string }

func gitPkt(s string) []byte {
	p, err := pktLine([]byte(s))
	if err != nil {
		panic(err)
	}
	return p
}
func gitSideband(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		out.Write(gitPkt(string(append([]byte{1}, data[:n]...))))
		data = data[n:]
	}
	return out.Bytes()
}
func gitTag(o GitObject) string {
	for _, line := range strings.Split(string(o.Content), "\n") {
		if strings.HasPrefix(line, "object ") && gitOID.MatchString(line[7:]) {
			return line[7:]
		}
	}
	panic("Invalid tag")
}
func gitPeel(objects map[string]GitObject, sha string) string {
	seen := map[string]bool{}
	for objects[sha].Type == "tag" {
		if seen[sha] {
			panic("Cyclic tag")
		}
		seen[sha] = true
		sha = gitTag(objects[sha])
	}
	return sha
}
func gitRefs(repo *Repo) []gitRef {
	refs := []gitRef{}
	for name, oid := range repo.Refs {
		if strings.HasPrefix(name, "refs/") {
			refs = append(refs, gitRef{name, oid})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}
func gitResult(w http.ResponseWriter, service string, data []byte) {
	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (a *App) serveGitRoutes(w http.ResponseWriter, r *http.Request, parts []string, body []byte) (handled bool) {
	advertisement := len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" && r.Method == "GET"
	upload := len(parts) == 3 && parts[2] == "git-upload-pack" && r.Method == "POST"
	receive := len(parts) == 3 && parts[2] == "git-receive-pack" && r.Method == "POST"
	if !advertisement && !upload && !receive {
		return false
	}
	handled = true
	defer func() {
		if recover() != nil {
			sendJSON(w, 400, map[string]string{"message": "Bad Request"})
		}
	}()
	owner, err := url.PathUnescape(parts[0])
	if err != nil {
		panic(err)
	}
	name, err := url.PathUnescape(parts[1])
	if err != nil {
		panic(err)
	}
	a.store.Mu.RLock()
	repo := a.store.Repos[repoKey(owner, strings.TrimSuffix(name, ".git"))]
	if owner != ViewerLogin {
		repo = nil
	}
	a.store.Mu.RUnlock()
	if repo == nil {
		sendJSON(w, 404, map[string]string{"message": "Not Found"})
		return true
	}
	if receive {
		a.gitReceive(w, repo, body)
		return true
	}
	if advertisement {
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			sendJSON(w, 403, map[string]string{"message": "Forbidden"})
			return true
		}
		var out bytes.Buffer
		a.store.Mu.RLock()
		func() {
			defer a.store.Mu.RUnlock()
			if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
				for _, s := range []string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"} {
					out.Write(gitPkt(s))
				}
			} else {
				caps := "side-band-64k ofs-delta shallow"
				if service == "git-receive-pack" {
					caps = "report-status side-band-64k delete-refs ofs-delta"
				}
				caps += " symref=HEAD:" + repo.headTarget()
				out.Write(gitPkt("# service=" + service + "\n"))
				out.Write(pktFlush())
				refs := []gitRef{}
				for _, ref := range gitRefs(repo) {
					if strings.HasPrefix(ref.name, "refs/heads/") || strings.HasPrefix(ref.name, "refs/tags/") {
						refs = append(refs, ref)
					}
				}
				if len(refs) == 0 {
					out.Write(gitPkt(gitZero + " capabilities^{}\x00" + caps + "\n"))
				} else {
					tip := repo.Refs[repo.headTarget()]
					if tip == "" {
						tip = gitZero
					}
					out.Write(gitPkt(tip + " HEAD\x00" + caps + "\n"))
					for _, ref := range refs {
						out.Write(gitPkt(ref.oid + " " + ref.name + "\n"))
						if strings.HasPrefix(ref.name, "refs/tags/") && repo.Objects[ref.oid].Type == "tag" {
							out.Write(gitPkt(gitPeel(repo.Objects, ref.oid) + " " + ref.name + "^{}\n"))
						}
					}
				}
			}
		}()
		out.Write(pktFlush())
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		_, _ = w.Write(out.Bytes())
		return true
	}
	a.gitUpload(w, r, repo, body)
	return true
}

// Traversal preserves pack.ts's breadth-first commit and then tree order.
func gitCollect(objects map[string]GitObject, wants []string, haves, boundary map[string]bool) []string {
	excluded := map[string]bool{}
	queue := []string{}
	for sha := range haves {
		queue = append(queue, sha)
	}
	for i := 0; i < len(queue); i++ {
		sha := queue[i]
		if excluded[sha] {
			continue
		}
		excluded[sha] = true
		if o := objects[sha]; o.Type == "commit" {
			queue = append(queue, parseCommit(o.Content).Parents...)
		}
	}
	included := map[string]bool{}
	visited := map[string]bool{}
	result := []string{}
	trees := []string{}
	queue = append([]string{}, wants...)
	add := func(sha string) {
		if !included[sha] {
			included[sha] = true
			result = append(result, sha)
		}
	}
	for i := 0; i < len(queue); i++ {
		sha := queue[i]
		if visited[sha] {
			continue
		}
		visited[sha] = true
		o, ok := objects[sha]
		if !ok {
			panic("Missing object " + sha)
		}
		switch o.Type {
		case "commit":
			if excluded[sha] {
				continue
			}
			add(sha)
			c := parseCommit(o.Content)
			trees = append(trees, c.Tree)
			if !boundary[sha] {
				queue = append(queue, c.Parents...)
			}
		case "tag":
			add(sha)
			queue = append(queue, gitTag(o))
		default:
			trees = append(trees, sha)
		}
	}
	for i := 0; i < len(trees); i++ {
		sha := trees[i]
		if included[sha] {
			continue
		}
		o, ok := objects[sha]
		if !ok {
			panic("Missing object " + sha)
		}
		add(sha)
		if o.Type == "tree" {
			for _, e := range parseTree(o.Content) {
				if e.Mode != "160000" {
					trees = append(trees, e.SHA)
				}
			}
		}
	}
	return result
}
func gitBoundary(objects map[string]GitObject, wants []string, depth int) []string {
	type step struct {
		sha      string
		distance int
	}
	queue := []step{}
	for _, sha := range wants {
		queue = append(queue, step{sha, 1})
	}
	distances := map[string]int{}
	tags := map[string]bool{}
	order := []string{}
	for i := 0; i < len(queue); i++ {
		s := queue[i]
		o := objects[s.sha]
		if o.Type == "tag" {
			if !tags[s.sha] {
				tags[s.sha] = true
				queue = append(queue, step{gitTag(o), s.distance})
			}
			continue
		}
		if o.Type != "commit" || distances[s.sha] > 0 && distances[s.sha] <= s.distance {
			continue
		}
		if distances[s.sha] == 0 {
			order = append(order, s.sha)
		}
		distances[s.sha] = s.distance
		if s.distance < depth {
			for _, p := range parseCommit(o.Content).Parents {
				queue = append(queue, step{p, s.distance + 1})
			}
		}
	}
	result := []string{}
	for _, sha := range order {
		if distances[sha] == depth && len(parseCommit(objects[sha].Content).Parents) > 0 {
			result = append(result, sha)
		}
	}
	return result
}
func gitSet(lines []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range lines {
		out[s] = true
	}
	return out
}

// Objects are immutable after insertion; copying the index permits compression
// without retaining the store lock or racing with a push.
func (a *App) gitSnapshot(repo *Repo) map[string]GitObject {
	a.store.Mu.RLock()
	defer a.store.Mu.RUnlock()
	objects := make(map[string]GitObject, len(repo.Objects))
	for sha, object := range repo.Objects {
		objects[sha] = object
	}
	return objects
}

func gitFetchSelection(objects map[string]GitObject, wants []string, haves, shallow map[string]bool, depth int) ([]string, []string) {
	for _, sha := range wants {
		if _, ok := objects[sha]; !ok {
			panic("Unknown want")
		}
	}
	known := map[string]bool{}
	for sha := range haves {
		if _, ok := objects[sha]; ok {
			known[sha] = true
		}
	}
	boundary := map[string]bool{}
	var boundaryOrder []string
	if depth > 0 {
		tips := wants
		if len(shallow) > 0 {
			tips = nil
			for sha := range shallow {
				tips = append(tips, sha)
			}
			sort.Strings(tips)
		}
		boundaryOrder = gitBoundary(objects, tips, depth)
		boundary = gitSet(boundaryOrder)
	}
	exclude := known
	if depth > 0 || len(shallow) > 0 {
		exclude = map[string]bool{}
	}
	raw := gitCollect(objects, wants, exclude, boundary)
	tips := []string{}
	for sha := range known {
		tips = append(tips, sha)
	}
	closure := gitSet(gitCollect(objects, tips, map[string]bool{}, shallow))
	delivered := []string{}
	for _, sha := range raw {
		if !closure[sha] {
			delivered = append(delivered, sha)
		}
	}
	lines := []string{}
	for _, sha := range boundaryOrder {
		lines = append(lines, "shallow "+sha+"\n")
	}
	deliveredSet := gitSet(delivered)
	for sha := range shallow {
		if boundary[sha] || objects[sha].Type != "commit" {
			continue
		}
		complete := true
		for _, parent := range parseCommit(objects[sha].Content).Parents {
			if !deliveredSet[parent] && !haves[parent] {
				complete = false
			}
		}
		if complete {
			lines = append(lines, "unshallow "+sha+"\n")
		}
	}
	return delivered, lines
}

func (a *App) gitFetchPack(repo *Repo, wants []string, haves, shallow map[string]bool, depth int) ([]byte, []string) {
	// A single tip and no existing objects is the common shallow-clone request.
	key := ""
	if depth > 0 && len(wants) == 1 && len(haves) == 0 && len(shallow) == 0 {
		key = fmt.Sprintf("%s:%d", wants[0], depth)
	}
	var entry *gitPackEntry
	if key != "" {
		repo.gitCacheMu.Lock()
		if repo.gitCache == nil {
			repo.gitCache = make(map[string]*gitPackEntry)
		}
		if cached := repo.gitCache[key]; cached != nil {
			repo.gitCacheMu.Unlock()
			<-cached.ready
			if cached.err != nil {
				panic(cached.err)
			}
			return cached.pack, cached.lines
		}
		entry = &gitPackEntry{ready: make(chan struct{})}
		repo.gitCache[key] = entry
		repo.gitCacheMu.Unlock()
	}
	var pack []byte
	var lines []string
	var err error
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
		}()
		objects := a.gitSnapshot(repo)
		var shas []string
		shas, lines = gitFetchSelection(objects, wants, haves, shallow, depth)
		pack, err = buildPack(shas, objects)
	}()
	if entry != nil {
		entry.pack, entry.lines, entry.err = pack, lines, err
		close(entry.ready)
		if err != nil {
			repo.gitCacheMu.Lock()
			delete(repo.gitCache, key)
			repo.gitCacheMu.Unlock()
		}
	}
	if err != nil {
		panic(err)
	}
	return pack, lines
}

func (a *App) gitUpload(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	items, err := parsePktLines(body, 0)
	if err != nil {
		panic(err)
	}
	v2 := r.Header.Get("Git-Protocol") == "version=2"
	args := []string{}
	start := 0
	if v2 {
		delim := -1
		for i, item := range items {
			if item.Type == "delim" {
				delim = i
				break
			}
		}
		if len(items) == 0 || items[0].Type != "line" || delim < 1 {
			panic("Missing command")
		}
		start = delim + 1
	}
	for _, item := range items[start:] {
		if item.Type == "line" {
			args = append(args, strings.TrimSuffix(string(item.Data), "\n"))
		}
	}
	if v2 {
		switch strings.TrimSuffix(string(items[0].Data), "\n") {
		case "command=ls-refs":
			a.store.Mu.RLock()
			defer a.store.Mu.RUnlock()
			refs := gitRefs(repo)
			options := gitSet(args)
			if tip := repo.Refs[repo.headTarget()]; tip != "" || options["unborn"] {
				if tip == "" {
					tip = "unborn"
				}
				refs = append([]gitRef{{"HEAD", tip}}, refs...)
			}
			prefixes := []string{}
			for _, arg := range args {
				if strings.HasPrefix(arg, "ref-prefix ") {
					prefixes = append(prefixes, arg[11:])
				}
			}
			var out bytes.Buffer
			for _, ref := range refs {
				matches := len(prefixes) == 0
				for _, prefix := range prefixes {
					if strings.HasPrefix(ref.name, prefix) {
						matches = true
					}
				}
				if !matches {
					continue
				}
				line := ref.oid + " " + ref.name
				if ref.name == "HEAD" && options["symrefs"] {
					line += " symref-target:" + repo.headTarget()
				}
				if options["peel"] && repo.Objects[ref.oid].Type == "tag" {
					line += " peeled:" + gitPeel(repo.Objects, ref.oid)
				}
				out.Write(gitPkt(line + "\n"))
			}
			out.Write(pktFlush())
			gitResult(w, "git-upload-pack", out.Bytes())
			return
		case "command=fetch":
		default:
			panic("Unknown command")
		}
	}
	wants := []string{}
	haves, shallow := map[string]bool{}, map[string]bool{}
	depth := 0
	for _, arg := range args {
		fields := strings.Split(arg, " ")
		key, value := fields[0], ""
		if len(fields) > 1 {
			value = fields[1]
		}
		switch key {
		case "want", "have", "shallow":
			if !gitOID.MatchString(value) {
				panic("Invalid oid")
			}
			switch key {
			case "want":
				wants = append(wants, value)
			case "have":
				haves[value] = true
			case "shallow":
				shallow[value] = true
			}
		case "deepen":
			if value == "" || value[0] < '1' || value[0] > '9' {
				panic("Invalid depth")
			}
			for _, c := range value {
				if c < '0' || c > '9' {
					panic("Invalid depth")
				}
			}
			n, err := strconv.ParseUint(value, 10, 53)
			if err != nil || n > 1<<53-1 {
				panic("Invalid depth")
			}
			depth = int(n)
		case "thin-pack", "no-progress", "ofs-delta", "done":
		default:
			panic("Invalid argument")
		}
	}
	if len(wants) == 0 {
		panic("Missing want")
	}
	pack, lines := a.gitFetchPack(repo, wants, haves, shallow, depth)
	var out bytes.Buffer
	if v2 {
		out.Write(gitPkt("acknowledgments\n"))
		out.Write(gitPkt("NAK\n"))
		out.Write(pktDelim())
	} else {
		out.Write(gitPkt("NAK\n"))
	}
	if v2 && (depth > 0 || len(shallow) > 0) {
		out.Write(gitPkt("shallow-info\n"))
	}
	if !v2 || depth > 0 || len(shallow) > 0 {
		for _, line := range lines {
			out.Write(gitPkt(line))
		}
	}
	if v2 && (depth > 0 || len(shallow) > 0) {
		out.Write(pktDelim())
	}
	if v2 {
		out.Write(gitPkt("packfile\n"))
	}
	out.Write(gitSideband(pack))
	out.Write(pktFlush())
	gitResult(w, "git-upload-pack", out.Bytes())
}

// Stop as soon as the old tip is found, rather than enumerating its history.
func gitIsAncestor(objects map[string]GitObject, old, tip string) bool {
	queue := []string{tip}
	seen := map[string]bool{}
	for i := 0; i < len(queue); i++ {
		sha := queue[i]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		object := objects[sha]
		switch object.Type {
		case "tag":
			queue = append(queue, gitTag(object))
		case "commit":
			if sha == old {
				return true
			}
			queue = append(queue, parseCommit(object.Content).Parents...)
		}
	}
	return false
}

func (a *App) gitReceive(w http.ResponseWriter, repo *Repo, body []byte) {
	type update struct{ old, next, ref string }
	commands := []update{}
	capabilities := map[string]bool{}
	offset := 0
	for {
		if len(body)-offset < 4 {
			panic("Invalid packet")
		}
		prefix := body[offset : offset+4]
		for _, c := range prefix {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				panic("Invalid packet")
			}
		}
		length, _ := strconv.ParseUint(string(prefix), 16, 16)
		offset += 4
		if length == 0 {
			break
		}
		if length < 4 || int(length)-4 > len(body)-offset {
			panic("Invalid packet")
		}
		parts := strings.Split(strings.TrimSuffix(string(body[offset:offset+int(length)-4]), "\n"), "\x00")
		offset += int(length) - 4
		if len(parts) > 2 || len(commands) > 0 && len(parts) > 1 {
			panic("Invalid capabilities")
		}
		if len(parts) == 2 {
			capabilities = gitSet(strings.Split(parts[1], " "))
		}
		line := parts[0]
		if len(line) < 83 || line[40] != ' ' || line[81] != ' ' {
			panic("Invalid command")
		}
		old, next, ref := line[:40], line[41:81], line[82:]
		for i := 0; i < 40; i++ {
			for _, c := range [2]byte{old[i], next[i]} {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
					panic("Invalid command")
				}
			}
		}
		if strings.ContainsAny(ref, "\t\n\f\r \x00") {
			panic("Invalid command")
		}
		commands = append(commands, update{old, next, ref})
	}
	// Parsing and decompression touch only the incoming pack, never shared state.
	incoming := &GitStore{}
	unpack := "ok"
	if offset < len(body) {
		if _, err := parsePack(body[offset:], incoming); err != nil {
			unpack = strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
		}
	}
	var report bytes.Buffer
	func() {
		a.store.Mu.Lock()
		defer a.store.Mu.Unlock()
		if unpack == "ok" && len(incoming.Objects) > 0 {
			if repo.Objects == nil {
				repo.Objects = make(map[string]GitObject, len(incoming.Objects))
			}
			for sha, object := range incoming.Objects {
				repo.Objects[sha] = object
			}
		}
		report.Write(gitPkt("unpack " + unpack + "\n"))
		for _, command := range commands {
			current := repo.Refs[command.ref]
			if current == "" {
				current = gitZero
			}
			reason := ""
			switch {
			case unpack != "ok":
				reason = "unpack failed"
			case !strings.HasPrefix(command.ref, "refs/"):
				reason = "invalid ref"
			case command.old != current:
				reason = "old oid does not match"
			}
			if reason == "" && command.next != gitZero {
				next, ok := repo.Objects[command.next]
				if !ok {
					reason = "object not found"
				} else if command.old != gitZero && repo.Objects[command.old].Type == "commit" && next.Type == "commit" && !gitIsAncestor(repo.Objects, command.old, command.next) {
					reason = "non-fast-forward"
				}
			}
			if reason == "" {
				if command.next == gitZero {
					repo.deleteRef(command.ref)
				} else {
					repo.setRef(command.ref, command.next)
				}
				report.Write(gitPkt("ok " + command.ref + "\n"))
			} else {
				report.Write(gitPkt("ng " + command.ref + " " + reason + "\n"))
			}
		}
	}()
	var out bytes.Buffer
	if capabilities["report-status"] {
		if capabilities["side-band-64k"] {
			out.Write(gitSideband(report.Bytes()))
		} else {
			out.Write(report.Bytes())
		}
		out.Write(pktFlush())
	}
	gitResult(w, "git-receive-pack", out.Bytes())
}
