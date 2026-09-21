package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type fakeStore struct {
	mu       sync.Mutex
	seen     map[string]int
	failFor  string
	refreshC chan string
}

func newFakeStore() *fakeStore {
	return &fakeStore{seen: map[string]int{}, refreshC: make(chan string, 64)}
}

func (f *fakeStore) Refresh(_ context.Context, connID string) error {
	f.mu.Lock()
	f.seen[connID]++
	f.mu.Unlock()
	select {
	case f.refreshC <- connID:
	default:
	}
	if connID == f.failFor {
		return errors.New("redis down")
	}
	return nil
}

func (f *fakeStore) count(connID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[connID]
}

// Refreshing several times per TTL means one missed cycle does not expire
// live interests.
func TestRefresherIntervalIsAFractionOfTTL(t *testing.T) {
	r := newRefresher(newFakeStore(), 90*time.Second, quiet())
	if r.interval != 30*time.Second {
		t.Fatalf("interval = %v, want a third of the TTL", r.interval)
	}
	// A tiny TTL must not produce a busy loop.
	if got := newRefresher(newFakeStore(), time.Millisecond, quiet()).interval; got < time.Second {
		t.Fatalf("interval = %v, want a floor of at least a second", got)
	}
}

func TestRefresherTracksConnections(t *testing.T) {
	f := newFakeStore()
	r := newRefresher(f, 3*time.Second, quiet())

	r.add("conn-1")
	r.add("conn-2")
	if r.tracked() != 2 {
		t.Fatalf("tracked = %d, want 2", r.tracked())
	}
	r.remove("conn-1")
	if r.tracked() != 1 {
		t.Fatalf("tracked = %d, want 1", r.tracked())
	}

	r.refreshAll(context.Background())
	if f.count("conn-1") != 0 {
		t.Error("a removed connection was still refreshed")
	}
	if f.count("conn-2") != 1 {
		t.Errorf("conn-2 refreshed %d times, want 1", f.count("conn-2"))
	}
}

// One connection's Redis error must not stop the others being kept alive.
func TestRefresherContinuesPastFailures(t *testing.T) {
	f := newFakeStore()
	f.failFor = "broken"
	r := newRefresher(f, 3*time.Second, quiet())
	r.add("broken")
	r.add("healthy")

	r.refreshAll(context.Background())

	if f.count("healthy") != 1 {
		t.Fatalf("healthy refreshed %d times; one failure aborted the sweep", f.count("healthy"))
	}
}

func TestRefresherRunsOnTicker(t *testing.T) {
	f := newFakeStore()
	r := newRefresher(f, 3*time.Second, quiet()) // 1s floor
	r.add("conn-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()

	select {
	case <-f.refreshC:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh within 5s; the ticker never fired")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return on context cancellation")
	}
}
