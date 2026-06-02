package emitter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ghwebhooks "github.com/go-playground/webhooks/v6/github"

	"github.com/rancher/gitsim/pkg/core"
	"github.com/rancher/gitsim/pkg/emitter"
	"github.com/rancher/gitsim/pkg/provider/github"
)

// pushAfterServer returns an httptest.Server that records the "after" SHA from
// each incoming GitHub push webhook body, in arrival order, on the returned channel.
func pushAfterServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	ch := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pl struct {
			After string `json:"after"`
		}
		if err := json.NewDecoder(r.Body).Decode(&pl); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ch <- pl.After
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func newEvent(after string) core.PushEvent {
	return core.PushEvent{
		Host:   "github.com",
		Owner:  "acme",
		Repo:   "myrepo",
		Branch: "main",
		Before: "0000000000000000000000000000000000000000",
		After:  after,
	}
}

// TestSend_CorrectSecret verifies the full round-trip: emitter builds a signed
// request that the go-playground/webhooks library (same as Fleet uses) accepts.
func TestSend_CorrectSecret(t *testing.T) {
	const secret = "s3cr3t"
	hook, err := ghwebhooks.New(ghwebhooks.Options.Secret(secret))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var parsed ghwebhooks.PushPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := hook.Parse(r, ghwebhooks.PushEvent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		parsed = payload.(ghwebhooks.PushPayload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	e := emitter.New(core.RealClock{})
	result, err := e.Send(context.Background(), srv.URL, emitter.Delivery{
		Provider: github.Provider{},
		Event:    newEvent("abc123def456"),
		Secret:   secret,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body: %s", result.StatusCode, result.Body)
	}

	mu.Lock()
	after := parsed.After
	mu.Unlock()
	if after != "abc123def456" {
		t.Errorf("After: got %q want abc123def456", after)
	}
}

// TestSend_WrongSecret verifies that a tampered signature causes the webhook
// library to reject the payload (simulating Fleet's HMAC check).
func TestSend_WrongSecret(t *testing.T) {
	const correctSecret = "correct"
	hook, err := ghwebhooks.New(ghwebhooks.Options.Secret(correctSecret))
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := hook.Parse(r, ghwebhooks.PushEvent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	e := emitter.New(core.RealClock{})
	result, err := e.Send(context.Background(), srv.URL, emitter.Delivery{
		Provider: github.Provider{},
		Event:    newEvent("deadbeef"),
		Secret:   "wrong-secret",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong secret, got %d", result.StatusCode)
	}
}

// TestSendBatch_ArrivalOrder verifies that deliveries arrive at the server in
// ascending SendAt order regardless of their position in the input slice, and
// that results are returned indexed to match the input slice.
func TestSendBatch_ArrivalOrder(t *testing.T) {
	srv, arrived := pushAfterServer(t)

	e := emitter.New(core.RealClock{})

	// Input order: sha1 (10ms), sha2 (0), sha3 (5ms).
	// Expected arrival order: sha2, sha3, sha1.
	deliveries := []emitter.Delivery{
		{Provider: github.Provider{}, Event: newEvent("sha1"), SendAt: 10 * time.Millisecond},
		{Provider: github.Provider{}, Event: newEvent("sha2"), SendAt: 0},
		{Provider: github.Provider{}, Event: newEvent("sha3"), SendAt: 5 * time.Millisecond},
	}

	results, err := e.SendBatch(context.Background(), srv.URL, deliveries)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	wantOrder := []string{"sha2", "sha3", "sha1"}
	for i, want := range wantOrder {
		got := <-arrived
		if got != want {
			t.Errorf("arrival[%d]: got %q want %q", i, got, want)
		}
	}

	// results[i] must correspond to deliveries[i].
	wantResults := []string{"sha1", "sha2", "sha3"}
	for i, want := range wantResults {
		if got := results[i].Delivery.Event.After; got != want {
			t.Errorf("results[%d].After: got %q want %q", i, got, want)
		}
		if results[i].StatusCode != http.StatusOK {
			t.Errorf("results[%d].StatusCode: got %d want 200", i, results[i].StatusCode)
		}
	}
}

// TestSendBatch_ManualClock_BlocksUntilAdvanced verifies that a batch delivery
// with a non-zero SendAt blocks until the injected ManualClock is advanced past
// the threshold, making timing deterministic in tests.
func TestSendBatch_ManualClock_BlocksUntilAdvanced(t *testing.T) {
	srv, arrived := pushAfterServer(t)

	clock := core.NewManualClock(time.Unix(1_000_000, 0))
	e := emitter.New(clock)

	const delay = 100 * time.Millisecond
	deliveries := []emitter.Delivery{
		{Provider: github.Provider{}, Event: newEvent("early"), SendAt: 0},
		{Provider: github.Provider{}, Event: newEvent("late"), SendAt: delay},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		_, err := e.SendBatch(ctx, srv.URL, deliveries)
		errc <- err
	}()

	// "early" (SendAt:0) must arrive without any clock advancement.
	select {
	case got := <-arrived:
		if got != "early" {
			t.Fatalf("first arrival: got %q want early", got)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for early delivery")
	}

	// "late" must NOT arrive before the clock is advanced: give the batch
	// goroutine real time to block in clock.After, then check.
	time.Sleep(15 * time.Millisecond)
	select {
	case got := <-arrived:
		t.Fatalf("late delivery arrived before clock advance: %q", got)
	default:
	}

	// Advance past the threshold → "late" must now arrive.
	clock.Advance(delay)
	select {
	case got := <-arrived:
		if got != "late" {
			t.Fatalf("second arrival: got %q want late", got)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for late delivery after clock advance")
	}

	if err := <-errc; err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}
