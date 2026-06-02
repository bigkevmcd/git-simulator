package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/rancher/gitsim/pkg/core"
	"github.com/rancher/gitsim/pkg/store"
)

var staticFiles = map[string][]byte{
	"README.md":   []byte("# hello"),
	"src/main.go": []byte("package main"),
	"src/util.go": []byte("package main\nfunc util() {}"),
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)
	content := core.NewStaticContent(staticFiles)
	return store.New(mc, content)
}

func newTestStoreWithClock(t *testing.T) (*store.Store, *core.ManualClock) {
	t.Helper()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)
	content := core.NewStaticContent(staticFiles)
	return store.New(mc, content), mc
}

// TestCreateAndGetRepo verifies basic repo lifecycle.
func TestCreateAndGetRepo(t *testing.T) {
	s := newTestStore(t)

	r, err := s.CreateRepo("github.com", "acme", "widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	h, o, n := r.Identity()
	if h != "github.com" || o != "acme" || n != "widget" {
		t.Fatalf("unexpected identity: %s/%s/%s", h, o, n)
	}

	got, err := s.GetRepo("github.com", "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}
	if got != r {
		t.Fatal("GetRepo returned different pointer than CreateRepo")
	}
}

// TestCreateRepo_DuplicateIsError ensures ErrAlreadyExists on second create.
func TestCreateRepo_DuplicateIsError(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateRepo("github.com", "acme", "widget", "main"); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateRepo("github.com", "acme", "widget", "main")
	if !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

// TestGetRepo_NotFound returns ErrNotFound for missing repos.
func TestGetRepo_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetRepo("github.com", "nobody", "nothing")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestInitialCommitIsVisible checks that CreateRepo seeds a cloneable initial commit.
func TestInitialCommitIsVisible(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	sha, ok := r.VisibleCommit("main")
	if !ok || sha == "" {
		t.Fatal("expected visible commit after CreateRepo")
	}

	// Verify the commit object exists in the storer and has a valid tree.
	storer := r.Storer()
	hash := plumbing.NewHash(sha)
	obj, err := storer.EncodedObject(plumbing.CommitObject, hash)
	if err != nil {
		t.Fatalf("commit object not in storer: %v", err)
	}
	commit, err := object.DecodeCommit(storer, obj)
	if err != nil {
		t.Fatalf("decode commit: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	// Expect README.md at root
	_, err = tree.File("README.md")
	if err != nil {
		t.Fatalf("README.md not in tree: %v", err)
	}
	// Expect src/main.go
	_, err = tree.File("src/main.go")
	if err != nil {
		t.Fatalf("src/main.go not in tree: %v", err)
	}
}

// TestAddCommitDelay is the core race-condition test:
// AddCommit with delay > 0 should keep old SHA visible until clock advances.
func TestAddCommitDelay(t *testing.T) {
	s, mc := newTestStoreWithClock(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	oldSHA, _ := r.VisibleCommit("main")

	newSHA, err := r.AddCommit("main", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if newSHA == oldSHA {
		t.Fatal("AddCommit returned same SHA as parent")
	}

	// Before delay elapses: visible should still be oldSHA.
	vis, _ := r.VisibleCommit("main")
	if vis != oldSHA {
		t.Fatalf("before promotion: want %s got %s", oldSHA, vis)
	}

	// Also check via Store.VisibleCommit (CommitResolver interface).
	if sha, ok := s.VisibleCommit("github.com", "acme", "widget", "main"); !ok || sha != oldSHA {
		t.Fatalf("Store.VisibleCommit before promotion: want %s got %s (ok=%v)", oldSHA, sha, ok)
	}

	// Advance clock past the delay.
	mc.Advance(600 * time.Millisecond)

	vis, _ = r.VisibleCommit("main")
	if vis != newSHA {
		t.Fatalf("after promotion: want %s got %s", newSHA, vis)
	}
}

// TestAddCommitImmediatePromotion checks that delay==0 is visible right away.
func TestAddCommitImmediatePromotion(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	oldSHA, _ := r.VisibleCommit("main")

	newSHA, err := r.AddCommit("main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if newSHA == oldSHA {
		t.Fatal("AddCommit returned same SHA as parent")
	}

	vis, ok := r.VisibleCommit("main")
	if !ok || vis != newSHA {
		t.Fatalf("want %s visible immediately, got %s (ok=%v)", newSHA, vis, ok)
	}
}

// TestAddCommit_ParentChain verifies that multiple commits chain correctly.
func TestAddCommit_ParentChain(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	sha0, _ := r.VisibleCommit("main")

	sha1, _ := r.AddCommit("main", 0)
	sha2, _ := r.AddCommit("main", 0)

	storer := r.Storer()

	// sha2's parent should be sha1
	obj, _ := storer.EncodedObject(plumbing.CommitObject, plumbing.NewHash(sha2))
	c2, _ := object.DecodeCommit(storer, obj)
	if len(c2.ParentHashes) != 1 || c2.ParentHashes[0].String() != sha1 {
		t.Fatalf("sha2 parent: want %s, got %v", sha1, c2.ParentHashes)
	}

	// sha1's parent should be sha0 (initial commit)
	obj, _ = storer.EncodedObject(plumbing.CommitObject, plumbing.NewHash(sha1))
	c1, _ := object.DecodeCommit(storer, obj)
	if len(c1.ParentHashes) != 1 || c1.ParentHashes[0].String() != sha0 {
		t.Fatalf("sha1 parent: want %s, got %v", sha0, c1.ParentHashes)
	}
}

// TestAddCommit_RefAdvancesAfterPromotion verifies the storer ref is updated.
func TestAddCommit_RefAdvancesAfterPromotion(t *testing.T) {
	s, mc := newTestStoreWithClock(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	oldSHA, _ := r.VisibleCommit("main")

	newSHA, _ := r.AddCommit("main", 200*time.Millisecond)

	// Before promotion: storer ref should still point to oldSHA.
	ref, err := r.Storer().Reference(plumbing.ReferenceName("refs/heads/main"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash().String() != oldSHA {
		t.Fatalf("before promotion storer ref: want %s got %s", oldSHA, ref.Hash())
	}

	// Advance clock and call VisibleCommit to trigger lazy promotion.
	mc.Advance(300 * time.Millisecond)
	r.VisibleCommit("main")

	ref, err = r.Storer().Reference(plumbing.ReferenceName("refs/heads/main"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash().String() != newSHA {
		t.Fatalf("after promotion storer ref: want %s got %s", newSHA, ref.Hash())
	}
}

// TestConcurrentAddAndVisible verifies no data races under concurrent access.
func TestConcurrentAddAndVisible(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.AddCommit("main", 0)
		}()
		go func() {
			defer wg.Done()
			_, _ = r.VisibleCommit("main")
		}()
	}
	wg.Wait()
}

// TestAddTag_Lightweight checks that a lightweight tag is served via the storer.
func TestAddTag_Lightweight(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	commitSHA, _ := r.VisibleCommit("main")
	tagSHA, err := r.AddTag("v1.0", "main", false)
	if err != nil {
		t.Fatal(err)
	}
	if tagSHA != commitSHA {
		t.Fatalf("lightweight tag should equal commit SHA: want %s got %s", commitSHA, tagSHA)
	}

	ref, err := r.Storer().Reference(plumbing.ReferenceName("refs/tags/v1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash().String() != commitSHA {
		t.Fatalf("tag ref: want %s got %s", commitSHA, ref.Hash())
	}
}

// TestAddTag_Annotated checks that an annotated tag object is written and the ref points to it.
func TestAddTag_Annotated(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("github.com", "acme", "widget", "main")

	commitSHA, _ := r.VisibleCommit("main")
	tagObjSHA, err := r.AddTag("v1.0", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	// tagObjSHA should differ from commitSHA (it's the tag object, not the commit)
	if tagObjSHA == commitSHA {
		t.Fatal("annotated tag SHA should differ from commit SHA")
	}

	// Verify the tag object points to the commit
	obj, err := r.Storer().EncodedObject(plumbing.TagObject, plumbing.NewHash(tagObjSHA))
	if err != nil {
		t.Fatalf("tag object not in storer: %v", err)
	}
	tag, err := object.DecodeTag(r.Storer(), obj)
	if err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	if tag.Target.String() != commitSHA {
		t.Fatalf("tag target: want %s got %s", commitSHA, tag.Target)
	}
}

// TestBuildTree_GitSortOrder is a regression test for the git tree sort-order bug.
// Git sorts tree entries as if directory names have a trailing "/", which differs
// from plain lexicographic order when a directory name is a prefix of a file name
// (e.g. "charts" < "charts.yaml" in lex but "charts.yaml" < "charts/" in git).
// go-git's Tree.Encode returns ErrEntriesNotSorted if entries are in lex order.
func TestBuildTree_GitSortOrder(t *testing.T) {
	// "charts" (dir) and "charts.yaml" (file) at the same level:
	// lex sort → [charts, charts.yaml] (dir first)
	// git sort → [charts.yaml, charts/] (file first, because '.' < '/')
	problematic := map[string][]byte{
		"charts.yaml":        []byte("name: myapp"),
		"charts/values.yaml": []byte("replicaCount: 1"),
		"README.md":          []byte("# hello"),
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)
	s := store.New(mc, core.NewStaticContent(problematic))

	_, err := s.CreateRepo("github.com", "acme", "sort-test", "main")
	if err != nil {
		t.Fatalf("CreateRepo with dir/file prefix collision: %v", err)
	}
}

// TestList verifies that all created repos appear.
func TestList(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateRepo("github.com", "acme", "a", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepo("github.com", "acme", "b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepo("github.com", "acme", "c", "main"); err != nil {
		t.Fatal(err)
	}

	repos := s.List()
	if len(repos) != 3 {
		t.Fatalf("want 3 repos, got %d", len(repos))
	}
}

// TestVisibleCommit_UnknownRepo checks that the CommitResolver returns ok=false.
func TestVisibleCommit_UnknownRepo(t *testing.T) {
	s := newTestStore(t)
	sha, ok := s.VisibleCommit("github.com", "nobody", "nothing", "main")
	if ok || sha != "" {
		t.Fatalf("expected not-ok for unknown repo, got sha=%s ok=%v", sha, ok)
	}
}
