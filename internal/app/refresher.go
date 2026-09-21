package app

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// refreshable is the part of the interest store the refresher needs.
type refreshable interface {
	Refresh(ctx context.Context, connID string) error
}

// refresher keeps the TTL alive on interests belonging to connections
// this instance is actually holding.
//
// Interests expire so that an instance killed without running its
// disconnect hook does not leak them forever. That backstop needs
// something to distinguish "gone" from "quiet", and inbound traffic is
// the wrong signal: a subscriber that only listens sends no frames, and
// would expire while perfectly healthy.
//
// The right signal is connection ownership. This instance knows which
// sockets it holds, because the server tells it on connect and
// disconnect, so it refreshes exactly those. A connection that has gone
// away stops being refreshed by definition — the instance holding it
// either removed it or died with it.
type refresher struct {
	store    refreshable
	interval time.Duration
	log      *slog.Logger

	mu    sync.Mutex
	conns map[string]struct{}
}

func newRefresher(store refreshable, ttl time.Duration, log *slog.Logger) *refresher {
	// Refresh several times per TTL so a single missed cycle — a slow
	// Redis, a paused process — does not expire live interests.
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	return &refresher{
		store:    store,
		interval: interval,
		log:      log,
		conns:    make(map[string]struct{}),
	}
}

func (r *refresher) add(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[connID] = struct{}{}
}

func (r *refresher) remove(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, connID)
}

func (r *refresher) tracked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// run refreshes on a ticker until ctx is cancelled.
func (r *refresher) run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refreshAll(ctx)
		}
	}
}

// refreshAll refreshes every tracked connection.
//
// Failures are logged and skipped rather than aborting the sweep: one
// connection's Redis error must not stop every other connection's
// interests from being kept alive.
func (r *refresher) refreshAll(ctx context.Context) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.conns))
	for id := range r.conns {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	var failed int
	for _, id := range ids {
		if err := r.store.Refresh(ctx, id); err != nil {
			failed++
			r.log.Warn("refresh interests failed", "connID", id, "err", err.Error())
		}
	}
	if failed > 0 {
		r.log.Warn("interest refresh sweep had failures", "failed", failed, "total", len(ids))
	}
}
