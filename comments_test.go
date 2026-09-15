package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newCommentServer(t *testing.T, root string) *Server {
	t.Helper()
	ix := NewIndex(root)
	ix.Build()
	return NewServer(ix, nil)
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// --- store: load / save (Task 1) --------------------------------------------

func TestStoreLoadMissingReturnsEmpty(t *testing.T) {
	s := newStore(t.TempDir())
	cf, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cf.NextID != 1 || len(cf.Comments) != 0 {
		t.Fatalf("want empty NextID=1, got %+v", cf)
	}
}

func TestStoreSaveIsAtomicAndReadable(t *testing.T) {
	root := t.TempDir()
	s := newStore(root)
	in := commentsFile{NextID: 2, Comments: []Comment{{ID: "c1", File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "x", Body: "fix", Status: "open", CreatedAt: "t"}}}
	if err := s.save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(s.jsonPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind")
	}
	b, err := os.ReadFile(s.jsonPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got commentsFile
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.NextID != 2 || len(got.Comments) != 1 || got.Comments[0].ID != "c1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if filepath.Dir(s.jsonPath()) != s.dir() {
		t.Fatalf("json not under .px0")
	}
}

// --- store: CRUD (Task 2) ---------------------------------------------------

func TestStoreAddAssignsMonotonicIDs(t *testing.T) {
	s := newStore(t.TempDir())
	a, err := s.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "x", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.add(Comment{File: "b.go", LineStart: 2, LineEnd: 2, Snippet: "y", Body: "two"})
	if a.ID != "c1" || b.ID != "c2" {
		t.Fatalf("ids: %s %s", a.ID, b.ID)
	}
	if a.Status != "open" || a.CreatedAt == "" {
		t.Fatalf("defaults not set: %+v", a)
	}
}

func TestStoreResolveIsIdempotent(t *testing.T) {
	s := newStore(t.TempDir())
	c, _ := s.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "x", Body: "b"})
	n, _ := s.resolve(c.ID)
	if n != 1 {
		t.Fatalf("first resolve changed %d", n)
	}
	n, _ = s.resolve(c.ID)
	if n != 0 {
		t.Fatalf("second resolve changed %d, want 0", n)
	}
	got, _ := s.list()
	if got[0].Status != "resolved" || got[0].ResolvedAt == "" {
		t.Fatalf("not resolved: %+v", got[0])
	}
}

func TestStoreRemove(t *testing.T) {
	s := newStore(t.TempDir())
	c, _ := s.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "x", Body: "b"})
	if err := s.remove(c.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.list()
	if len(got) != 0 {
		t.Fatalf("want 0 after remove, got %d", len(got))
	}
}

// --- store: re-anchor + markdown (Task 3) -----------------------------------

func TestReanchorExactMatchNotStale(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\nfunc f() {}\n")
	s := newStore(root)
	c := Comment{File: "a.go", LineStart: 2, LineEnd: 2, Snippet: "func f() {}"}
	s.reanchor(&c)
	if c.Stale || c.LineStart != 2 {
		t.Fatalf("want not-stale at 2, got stale=%v line=%d", c.Stale, c.LineStart)
	}
}

func TestReanchorRebindsWhenMoved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "// added\n// lines\npackage main\nfunc f() {}\n")
	s := newStore(root)
	c := Comment{File: "a.go", LineStart: 2, LineEnd: 2, Snippet: "func f() {}"}
	s.reanchor(&c)
	if c.Stale || c.LineStart != 4 {
		t.Fatalf("want rebind to 4, got stale=%v line=%d", c.Stale, c.LineStart)
	}
}

func TestReanchorStaleWhenGone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newStore(root)
	c := Comment{File: "a.go", LineStart: 2, LineEnd: 2, Snippet: "func f() {}"}
	s.reanchor(&c)
	if !c.Stale {
		t.Fatalf("want stale, got not stale")
	}
}

func TestReanchorEmptySnippetNeverStale(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newStore(root)
	c := Comment{File: "a.go", LineStart: 99, LineEnd: 99, Snippet: "   "}
	s.reanchor(&c)
	if c.Stale {
		t.Fatalf("empty snippet should never be stale")
	}
}

func TestRenderMarkdownOpenOnlyWithIDs(t *testing.T) {
	md := renderReviewMarkdown([]Comment{
		{ID: "c1", File: "server.go", LineStart: 64, LineEnd: 64, Snippet: "x := 1", Body: "rename it", Status: "open"},
		{ID: "c2", File: "git.go", LineStart: 147, LineEnd: 151, Snippet: "func g() {}", Body: "handle empty", Status: "open", Stale: true},
		{ID: "c3", File: "z.go", LineStart: 1, LineEnd: 1, Snippet: "z", Body: "done", Status: "resolved"},
	})
	if !strings.Contains(md, "## [c1] server.go:64") {
		t.Fatalf("missing c1 heading:\n%s", md)
	}
	if !strings.Contains(md, "## [c2] git.go:147-151") || !strings.Contains(md, "STALE") {
		t.Fatalf("missing c2 stale heading:\n%s", md)
	}
	if strings.Contains(md, "c3") {
		t.Fatalf("resolved comment leaked into markdown:\n%s", md)
	}
	if !strings.Contains(md, "2 open") {
		t.Fatalf("header count wrong:\n%s", md)
	}
}

func TestRenderMarkdownZeroOpen(t *testing.T) {
	md := renderReviewMarkdown([]Comment{{ID: "c1", Status: "resolved"}})
	if !strings.Contains(md, "No open comments") {
		t.Fatalf("want empty body, got:\n%s", md)
	}
}

// --- exclude (Task 4) -------------------------------------------------------

func TestEnsureExcludedAppendsOnce(t *testing.T) {
	root := t.TempDir()
	info := filepath.Join(root, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "exclude"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ensureExcluded(root)
	ensureExcluded(root) // idempotent
	b, _ := os.ReadFile(filepath.Join(info, "exclude"))
	if strings.Count(string(b), ".px0/") != 1 {
		t.Fatalf("want exactly one .px0/ line, got:\n%s", b)
	}
}

func TestEnsureExcludedSkipsNonGit(t *testing.T) {
	root := t.TempDir()
	ensureExcluded(root)
	if _, err := os.Stat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git should not be created")
	}
}

// --- HTTP API (Task 5) ------------------------------------------------------

func TestAPIAddAndList(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\nfunc f() {}\n")
	s := newCommentServer(t, root)

	body := `{"file":"a.go","lineStart":2,"lineEnd":2,"snippet":"func f() {}","body":"rename"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/comments", bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST code %d: %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/api/comments", nil))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"id":"c1"`)) {
		t.Fatalf("GET code %d: %s", rec.Code, rec.Body)
	}
}

func TestAPIRejectsPathOutsideRoot(t *testing.T) {
	s := newCommentServer(t, t.TempDir())
	body := `{"file":"../secret","lineStart":1,"lineEnd":1,"snippet":"x","body":"b"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/comments", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for traversal, got %d", rec.Code)
	}
}

func TestAPIResolve(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newCommentServer(t, root)
	s.cm.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "package main", Body: "b"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/comments/resolve", bytes.NewBufferString(`{"id":"c1"}`)))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"resolved":1`)) {
		t.Fatalf("resolve code %d: %s", rec.Code, rec.Body)
	}
}

func TestAPIDelete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newCommentServer(t, root)
	s.cm.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "package main", Body: "b"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/comments?id=c1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete code %d", rec.Code)
	}
	got, _ := s.cm.list()
	if len(got) != 0 {
		t.Fatalf("want 0 after delete, got %d", len(got))
	}
}

// --- SSE stream (Task 6) ----------------------------------------------------

func TestStreamEmitsOnChange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newCommentServer(t, root)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/comments/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := make(chan struct{}, 8)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 && strings.Contains(string(buf[:n]), "event: change") {
				events <- struct{}{}
			}
			if err != nil {
				return
			}
		}
	}()

	// Prime event on connect.
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("no prime event")
	}
	// A write must produce a change event.
	if _, err := s.cm.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "package main", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(4 * time.Second):
		t.Fatal("no change event after add")
	}
}

// --- CLI (Task 7) -----------------------------------------------------------

func TestRunReviewJSON(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n")
	s := newStore(root)
	s.add(Comment{File: "a.go", LineStart: 1, LineEnd: 1, Snippet: "package main", Body: "b"})
	chdir(t, root)
	if code := runReview([]string{"--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestRunResolveUnknownIDNonZero(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	if code := runResolve([]string{"c999"}); code == 0 {
		t.Fatalf("want non-zero exit for unknown id")
	}
}
