package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Comment is a single review note attached to a range of lines in a source
// file. Stale is derived at read time (see store.reanchor) rather than trusted
// from disk.
type Comment struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	LineStart  int    `json:"lineStart"`
	LineEnd    int    `json:"lineEnd"`
	Snippet    string `json:"snippet"`
	Body       string `json:"body"`
	Status     string `json:"status"`
	Stale      bool   `json:"stale"`
	CreatedAt  string `json:"createdAt"`
	ResolvedAt string `json:"resolvedAt,omitempty"`
}

// commentsFile is the on-disk shape of .px0/comments.json.
type commentsFile struct {
	NextID   int       `json:"nextId"`
	Comments []Comment `json:"comments"`
}

// store persists comments for a single served repo root. There is intentionally
// no in-memory cache: every read re-reads the file so the CLI (which runs in a
// separate process, often with the server down) and the browser never diverge.
type store struct {
	root string
	mu   sync.Mutex
}

func newStore(root string) *store { return &store{root: root} }

func (s *store) dir() string      { return filepath.Join(s.root, ".px0") }
func (s *store) jsonPath() string { return filepath.Join(s.dir(), "comments.json") }
func (s *store) mdPath() string   { return filepath.Join(s.dir(), "review.md") }

// load reads comments.json. A missing file is not an error: it yields an empty
// set whose id counter starts at 1.
func (s *store) load() (commentsFile, error) {
	b, err := os.ReadFile(s.jsonPath())
	if os.IsNotExist(err) {
		return commentsFile{NextID: 1}, nil
	}
	if err != nil {
		return commentsFile{}, err
	}
	var cf commentsFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return commentsFile{}, err
	}
	if cf.NextID < 1 {
		cf.NextID = 1
	}
	return cf, nil
}

// save writes comments.json atomically (temp file + fsync + rename), then
// regenerates the agent-facing review.md and makes sure .px0/ is ignored in the
// target repo.
func (s *store) save(cf commentsFile) error {
	// Re-anchor before persisting so the stored line numbers and the agent-facing
	// review.md reflect the file's current state (Stale flags included).
	for i := range cf.Comments {
		s.reanchor(&cf.Comments[i])
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.jsonPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.jsonPath()); err != nil {
		return err
	}
	if err := writeMarkdown(s.mdPath(), cf.Comments); err != nil {
		return err
	}
	ensureExcluded(s.root)
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// add assigns the comment a fresh id, stamps it open, and persists.
func (s *store) add(c Comment) (Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf, err := s.load()
	if err != nil {
		return Comment{}, err
	}
	c.ID = fmt.Sprintf("c%d", cf.NextID)
	cf.NextID++
	c.Status = "open"
	c.CreatedAt = nowRFC3339()
	c.ResolvedAt = ""
	cf.Comments = append(cf.Comments, c)
	if err := s.save(cf); err != nil {
		return Comment{}, err
	}
	return c, nil
}

// resolve marks the named open comments resolved and returns how many changed.
// It is idempotent: resolving an already-resolved id changes nothing.
func (s *store) resolve(ids ...string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf, err := s.load()
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	changed := 0
	for i := range cf.Comments {
		c := &cf.Comments[i]
		if want[c.ID] && c.Status != "resolved" {
			c.Status = "resolved"
			c.ResolvedAt = nowRFC3339()
			changed++
		}
	}
	if changed > 0 {
		if err := s.save(cf); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

// remove deletes a comment outright.
func (s *store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf, err := s.load()
	if err != nil {
		return err
	}
	out := cf.Comments[:0]
	for _, c := range cf.Comments {
		if c.ID != id {
			out = append(out, c)
		}
	}
	cf.Comments = out
	return s.save(cf)
}

// list returns every comment (open and resolved) with Stale recomputed against
// the current file contents.
func (s *store) list() ([]Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf, err := s.load()
	if err != nil {
		return nil, err
	}
	for i := range cf.Comments {
		s.reanchor(&cf.Comments[i])
	}
	return cf.Comments, nil
}

// reanchor recomputes a comment's line range and Stale flag against the current
// file. The stored snippet is the anchor of record: line numbers drift as the
// code is edited, so we trust the snippet's exact text over the stored line
// numbers. Matching is by substring (not whole lines) so partial-line and
// multi-line selections both anchor correctly. Callers already hold s.mu;
// reanchor takes no lock.
func (s *store) reanchor(c *Comment) {
	if strings.TrimSpace(c.Snippet) == "" {
		c.Stale = false
		return
	}
	b, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(c.File)))
	if err != nil {
		c.Stale = true
		return
	}
	content := string(b)
	idx := strings.Index(content, c.Snippet)
	if idx < 0 {
		c.Stale = true
		return
	}
	c.LineStart = 1 + strings.Count(content[:idx], "\n")
	c.LineEnd = c.LineStart + strings.Count(c.Snippet, "\n")
	c.Stale = false
}

// mdLang maps a file extension to a Markdown fence language for review.md.
func mdLang(file string) string {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go":
		return "go"
	case ".js", ".mjs":
		return "js"
	case ".ts":
		return "ts"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".css":
		return "css"
	case ".html":
		return "html"
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	default:
		return ""
	}
}

// renderReviewMarkdown produces the agent-facing review.md content. Only open
// comments appear; each carries its id, location, snippet, a stale marker when
// the code moved, and the reviewer's body.
func renderReviewMarkdown(cs []Comment) string {
	var open []Comment
	for _, c := range cs {
		if c.Status == "open" {
			open = append(open, c)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# px0 review — %d open comment", len(open))
	if len(open) != 1 {
		b.WriteString("s")
	}
	b.WriteString("\n<!-- generated by px0; do not edit. Resolve with: px0 resolve <id> -->\n")
	if len(open) == 0 {
		b.WriteString("\nNo open comments.\n")
		return b.String()
	}
	for _, c := range open {
		loc := fmt.Sprintf("%s:%d", c.File, c.LineStart)
		if c.LineEnd > c.LineStart {
			loc = fmt.Sprintf("%s:%d-%d", c.File, c.LineStart, c.LineEnd)
		}
		stale := ""
		if c.Stale {
			stale = " (STALE — code moved; match on the snippet, not the line numbers)"
		}
		fmt.Fprintf(&b, "\n## [%s] %s%s\n", c.ID, loc, stale)
		fmt.Fprintf(&b, "```%s\n%s\n```\n", mdLang(c.File), c.Snippet)
		fmt.Fprintf(&b, "%s\n", c.Body)
	}
	return b.String()
}

func writeMarkdown(path string, cs []Comment) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(renderReviewMarkdown(cs)), 0o644)
}

// ensureExcluded adds ".px0/" to the target repo's .git/info/exclude so the
// sidecar never shows up as an untracked change. It edits only the local,
// untracked exclude file — never the repo's tracked .gitignore — and is a no-op
// when root is not a git repo. Best-effort: failures never block a save.
func ensureExcluded(root string) {
	info := filepath.Join(root, ".git", "info")
	if st, err := os.Stat(info); err != nil || !st.IsDir() {
		return // not a git repo/worktree
	}
	p := filepath.Join(info, "exclude")
	b, _ := os.ReadFile(p)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == ".px0/" {
			return
		}
	}
	out := string(b)
	if len(out) > 0 && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += ".px0/\n"
	_ = os.WriteFile(p, []byte(out), 0o644)
}

// --- HTTP handlers -----------------------------------------------------------

// handleComments serves GET (list, optional ?file= filter), POST (create), and
// DELETE (?id=) on /api/comments.
func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cs, err := s.cm.list()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if f := r.URL.Query().Get("file"); f != "" {
			out := cs[:0]
			for _, c := range cs {
				if c.File == f {
					out = append(out, c)
				}
			}
			cs = out
		}
		if cs == nil {
			cs = []Comment{}
		}
		writeCommentJSON(w, http.StatusOK, map[string]any{"comments": cs})
	case http.MethodPost:
		var in Comment
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		_, rel, ok := s.safePath(in.File)
		if !ok || rel == "" {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		in.File = rel // store the normalized repo-relative path
		c, err := s.cm.add(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeCommentJSON(w, http.StatusCreated, c)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := s.cm.remove(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCommentsResolve marks one or more comments resolved.
func (s *Server) handleCommentsResolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID  string   `json:"id"`
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ids := in.IDs
	if in.ID != "" {
		ids = append(ids, in.ID)
	}
	n, err := s.cm.resolve(ids...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeCommentJSON(w, http.StatusOK, map[string]any{"resolved": n})
}

// handleCommentsStream is a Server-Sent Events endpoint that emits a "change"
// event whenever comments.json changes on disk, so the browser can live-update
// when the agent resolves comments from the CLI. It uses a 1s mtime poll to
// avoid an external file-watch dependency.
func (s *Server) handleCommentsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() { fmt.Fprint(w, "event: change\ndata: {}\n\n"); fl.Flush() }
	mtime := func() int64 {
		if st, err := os.Stat(s.cm.jsonPath()); err == nil {
			return st.ModTime().UnixNano()
		}
		return 0
	}
	last := mtime()
	send() // prime the client

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if m := mtime(); m != last {
				last = m
				send()
			}
		}
	}
}

func writeCommentJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
