package github_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghwebhooks "github.com/go-playground/webhooks/v6/github"

	"github.com/rancher/gitsim/pkg/core"
	"github.com/rancher/gitsim/pkg/provider/github"
	"github.com/rancher/gitsim/pkg/store"
)

// --- Webhook tests ---

func TestBuildWebhook_Push_RoundTrip(t *testing.T) {
	const secret = "super-secret"
	const before = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const after = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	p := github.Provider{}
	event := core.PushEvent{
		Host:   "github.com",
		Owner:  "acme",
		Repo:   "widget",
		Branch: "main",
		Before: before,
		After:  after,
	}

	headers, body, err := p.BuildWebhook(event, secret)
	if err != nil {
		t.Fatalf("BuildWebhook: %v", err)
	}

	// Verify required headers are present.
	if got := headers.Get("X-GitHub-Event"); got != "push" {
		t.Fatalf("X-GitHub-Event: want push, got %q", got)
	}
	if headers.Get("X-GitHub-Delivery") == "" {
		t.Fatal("X-GitHub-Delivery must not be empty")
	}
	if sig := headers.Get("X-Hub-Signature-256"); !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("X-Hub-Signature-256: want sha256=..., got %q", sig)
	}

	// Round-trip: feed into the go-playground/webhooks parser (same as Fleet uses).
	hook, err := ghwebhooks.New(ghwebhooks.Options.Secret(secret))
	if err != nil {
		t.Fatalf("ghwebhooks.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	payload, err := hook.Parse(req, ghwebhooks.PushEvent)
	if err != nil {
		t.Fatalf("hook.Parse: %v", err)
	}

	push, ok := payload.(ghwebhooks.PushPayload)
	if !ok {
		t.Fatalf("expected PushPayload, got %T", payload)
	}
	if push.After != after {
		t.Errorf("After: want %s, got %s", after, push.After)
	}
	if push.Before != before {
		t.Errorf("Before: want %s, got %s", before, push.Before)
	}
	if push.Ref != "refs/heads/main" {
		t.Errorf("Ref: want refs/heads/main, got %s", push.Ref)
	}
	wantURL := p.RepoURL("github.com", "acme", "widget")
	if push.Repository.HTMLURL != wantURL {
		t.Errorf("Repository.HTMLURL: want %s, got %s", wantURL, push.Repository.HTMLURL)
	}
}

func TestBuildWebhook_Tag(t *testing.T) {
	p := github.Provider{}
	event := core.PushEvent{
		Host:  "github.com",
		Owner: "acme",
		Repo:  "widget",
		Tag:   "v1.0.0",
		After: "cccccccccccccccccccccccccccccccccccccccc",
	}
	headers, _, err := p.BuildWebhook(event, "")
	if err != nil {
		t.Fatalf("BuildWebhook: %v", err)
	}
	// No secret → no signature header.
	if headers.Get("X-Hub-Signature-256") != "" {
		t.Fatal("X-Hub-Signature-256 must be absent when secret is empty")
	}
}

func TestBuildWebhook_WrongSecret(t *testing.T) {
	p := github.Provider{}
	event := core.PushEvent{
		Host:   "github.com",
		Owner:  "acme",
		Repo:   "widget",
		Branch: "main",
		After:  "dddddddddddddddddddddddddddddddddddddddd",
	}

	headers, body, err := p.BuildWebhook(event, "correct-secret")
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with the signature to simulate a wrong secret.
	headers.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")

	hook, _ := ghwebhooks.New(ghwebhooks.Options.Secret("correct-secret"))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	_, err = hook.Parse(req, ghwebhooks.PushEvent)
	if err == nil {
		t.Fatal("expected HMAC verification error, got nil")
	}
}

func TestBuildWebhook_RefTag(t *testing.T) {
	p := github.Provider{}
	event := core.PushEvent{
		Host:  "github.com",
		Owner: "acme",
		Repo:  "widget",
		Tag:   "v2.0",
		After: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	headers, body, err := p.BuildWebhook(event, "")
	if err != nil {
		t.Fatal(err)
	}

	hook, _ := ghwebhooks.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	payload, err := hook.Parse(req, ghwebhooks.PushEvent)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	push := payload.(ghwebhooks.PushPayload)
	if push.Ref != "refs/tags/v2.0" {
		t.Errorf("Ref: want refs/tags/v2.0, got %s", push.Ref)
	}
}

// --- Commits API tests ---

func TestCommitsAPI_ReturnsSHA(t *testing.T) {
	// Start the server first so we know its host, then register repos under that host.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	mc := core.NewManualClock(time.Now())
	s := store.New(mc, core.NewStaticContent(map[string][]byte{"README.md": []byte("hi")}))
	r, _ := s.CreateRepo(host, "acme", "widget", "main")
	expectedSHA, _ := r.VisibleCommit("main")

	p := github.Provider{}
	p.APIRoutes(mux, s)

	resp, err := http.Get(srv.URL + "/repos/acme/widget/commits/main")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Without Accept: application/vnd.github.v3.sha → JSON fallback.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	// Now with the SHA accept header (what Fleet sends).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/acme/widget/commits/main", nil)
	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with Accept: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status with SHA Accept: want 200, got %d", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if got := strings.TrimSpace(string(body)); got != expectedSHA {
		t.Fatalf("commits API SHA: want %s, got %s", expectedSHA, got)
	}
}

func TestCommitsAPI_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	mc := core.NewManualClock(time.Now())
	s := store.New(mc, core.NewStaticContent(map[string][]byte{}))

	github.Provider{}.APIRoutes(mux, s)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/nobody/norepo/commits/main", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestRepoURL(t *testing.T) {
	p := github.Provider{}
	got := p.RepoURL("github.com", "acme", "widget")
	want := "https://github.com/acme/widget"
	if got != want {
		t.Fatalf("RepoURL: want %s, got %s", want, got)
	}
}
