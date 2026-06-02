package gitsim_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
	ghwebhooks "github.com/go-playground/webhooks/v6/github"

	"github.com/rancher/gitsim/pkg/core"
	"github.com/rancher/gitsim/pkg/gitsim"
	_ "github.com/rancher/gitsim/pkg/provider/github" // register the GitHub provider
)

// lsRemote performs a git ls-remote against rawURL and returns the ref map.
func lsRemote(t *testing.T, rawURL string) map[plumbing.ReferenceName]plumbing.Hash {
	t.Helper()
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		URLs: []string{rawURL},
	})
	refs, err := remote.List(&gogit.ListOptions{})
	if err != nil {
		t.Fatalf("ls-remote %s: %v", rawURL, err)
	}
	m := make(map[plumbing.ReferenceName]plumbing.Hash, len(refs))
	for _, ref := range refs {
		m[ref.Name()] = ref.Hash()
	}
	return m
}

// TestSDK_RaceCondition is the headline end-to-end test.
// It pushes a commit with HeadDelay, fires a webhook immediately (before HEAD
// is promoted), and then verifies that ls-remote lags and then catches up —
// reproducing the Fleet issue #4837 race deterministically in-process.
func TestSDK_RaceCondition(t *testing.T) {
	const secret = "fleet-secret"
	const headDelay = 300 * time.Millisecond

	t0 := time.Unix(1_000_000, 0)
	clock := core.NewManualClock(t0)

	sim := gitsim.New(
		gitsim.WithClock(clock),
		gitsim.WithProviders("github"),
	)

	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	sim.SetBaseURL(srv.URL)

	repo, err := sim.CreateRepo("github", "acme", "app", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	oldSHA, ok := repo.VisibleCommit("main")
	if !ok {
		t.Fatal("initial commit not visible")
	}

	// Push a commit; HEAD stays at oldSHA for 300ms.
	newSHA, err := repo.Push(gitsim.OnBranch("main"), gitsim.HeadDelay(headDelay))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if newSHA == oldSHA {
		t.Fatal("Push returned the same SHA as the initial commit")
	}

	// ── Before promotion: ls-remote must still report the OLD sha. ──
	refs := lsRemote(t, repo.CloneURL())
	if got := refs["refs/heads/main"]; got.String() != oldSHA {
		t.Errorf("before promotion — ls-remote: got %s want %s", got, oldSHA)
	}

	// ── Webhook fires immediately with the NEW sha (the git object exists). ──
	// This is the race Fleet issue #4837 describes: the webhook arrives before
	// the remote's advertised HEAD reflects the new commit.
	var mu sync.Mutex
	var receivedAfter string

	hook, _ := ghwebhooks.New(ghwebhooks.Options.Secret(secret))
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := hook.Parse(r, ghwebhooks.PushEvent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		push, _ := payload.(ghwebhooks.PushPayload)
		mu.Lock()
		receivedAfter = push.After
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)

	result, err := sim.Webhook(repo, newSHA,
		gitsim.Target(receiver.URL),
		gitsim.SendAfter(0),
		gitsim.Secret(secret),
	)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("webhook receiver status %d, body: %s", result.StatusCode, result.Body)
	}

	mu.Lock()
	gotAfter := receivedAfter
	mu.Unlock()
	if gotAfter != newSHA {
		t.Errorf("webhook After: got %q want %q", gotAfter, newSHA)
	}

	// ls-remote still sees the OLD sha (clock not advanced yet).
	refs = lsRemote(t, repo.CloneURL())
	if got := refs["refs/heads/main"]; got.String() != oldSHA {
		t.Errorf("still before promotion — ls-remote: got %s want %s", got, oldSHA)
	}

	// ── Advance clock → HEAD promotes → ls-remote catches up. ──
	clock.Advance(headDelay + time.Millisecond)

	refs = lsRemote(t, repo.CloneURL())
	if got := refs["refs/heads/main"]; got.String() != newSHA {
		t.Errorf("after promotion — ls-remote: got %s want %s", got, newSHA)
	}
}

// TestSDK_HealthzAndControlAPI exercises the binary-facing HTTP interface:
// GET /healthz, POST /control/repos, POST /control/repos/{id}/commits.
func TestSDK_HealthzAndControlAPI(t *testing.T) {
	sim := gitsim.New(gitsim.WithProviders("github"))
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	sim.SetBaseURL(srv.URL)

	// /healthz must return 200.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: got %d", resp.StatusCode)
	}

	// Create a repo via the control API.
	body, _ := json.Marshal(map[string]any{
		"vendor":        "github",
		"owner":         "acme",
		"repo":          "ctrl-test",
		"defaultBranch": "main",
	})
	resp, err = http.Post(srv.URL+"/control/repos", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /control/repos: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: got %d", resp.StatusCode)
	}

	var created struct {
		ID       string `json:"id"`
		RepoURL  string `json:"repoURL"`
		CloneURL string `json:"cloneURL"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create-repo response: %v", err)
	}
	if created.ID != "github/acme/ctrl-test" {
		t.Errorf("id: got %q want github/acme/ctrl-test", created.ID)
	}
	if created.CloneURL == "" {
		t.Error("cloneURL must not be empty")
	}
	if created.RepoURL == "" {
		t.Error("repoURL must not be empty")
	}

	// Push a commit via the control API.
	body, _ = json.Marshal(map[string]any{"branch": "main"})
	resp2, err := http.Post(
		srv.URL+"/control/repos/github/acme/ctrl-test/commits",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST .../commits: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("add commit: got %d", resp2.StatusCode)
	}

	var commitResp struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&commitResp); err != nil {
		t.Fatalf("decode commit response: %v", err)
	}
	if commitResp.SHA == "" {
		t.Error("sha must not be empty")
	}

	// The new commit must be visible to a real ls-remote.
	refs := lsRemote(t, created.CloneURL)
	if got, ok := refs["refs/heads/main"]; !ok || got.String() != commitResp.SHA {
		t.Errorf("ls-remote after push: got %s want %s", got, commitResp.SHA)
	}
}

// TestSDK_GitHubCommitsAPI verifies that Fleet's polling path works:
// the GitHub commits REST endpoint returns the visible SHA.
func TestSDK_GitHubCommitsAPI(t *testing.T) {
	sim := gitsim.New(gitsim.WithProviders("github"))
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	sim.SetBaseURL(srv.URL)

	repo, err := sim.CreateRepo("github", "acme", "poll-test", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	initialSHA, _ := repo.VisibleCommit("main")

	// GET the commits API with the Fleet-style Accept header.
	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/repos/acme/poll-test/commits/main", nil)
	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET commits API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commits API status: %d", resp.StatusCode)
	}
	var buf [64]byte
	n, _ := resp.Body.Read(buf[:])
	if got := string(buf[:n]); got != initialSHA {
		t.Errorf("commits API SHA: got %q want %q", got, initialSHA)
	}
}
