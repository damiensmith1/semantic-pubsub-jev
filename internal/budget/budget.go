// Package budget caps what the judge is allowed to spend.
//
// Two limits, because they catch different failures:
//
//   - A rate cap stops a loop bug in seconds. A publish loop, a retry
//     storm or a stuck test can issue requests as fast as the API will
//     take them, and a spend cap alone would let it run for minutes
//     before noticing.
//   - A spend cap stops a slow bleed. A dev server left running with the
//     judge attached costs nothing dramatic per minute and a great deal
//     per weekend.
//
// Both fail the call rather than blocking, so that a tripped limit
// surfaces through the broker's failure policy — routing falls back to
// ordinary topic delivery — instead of stalling every publisher behind a
// queue that will not drain.
package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
)

// Sentinel errors, so a caller can tell a refusal from a failure.
var (
	// ErrSpendExceeded means the cumulative cost ceiling was reached.
	ErrSpendExceeded = errors.New("budget: spend limit reached")

	// ErrRateExceeded means too many calls were made too quickly.
	ErrRateExceeded = errors.New("budget: call rate limit reached")
)

// Asker is the part of the Jev client this wraps.
type Asker interface {
	Ask(ctx context.Context, req jev.Request) (*jev.Response, error)
}

// Options configures a Limiter.
type Options struct {
	// MaxSpendUSD is the cumulative ceiling for the process lifetime.
	// Zero disables the check, which is logged loudly at construction.
	MaxSpendUSD float64

	// MaxCallsPerMin bounds call rate. Zero disables the check.
	MaxCallsPerMin int

	Log *slog.Logger
}

// Limiter wraps an Asker and refuses calls that would breach a limit.
// Safe for concurrent use.
type Limiter struct {
	inner Asker
	opts  Options
	log   *slog.Logger

	mu sync.Mutex
	// spentUSD accumulates actual reported usage, not an estimate.
	spentUSD float64
	calls    int64
	tokens   int64
	// recent holds call times within the rate window, oldest first.
	recent  []time.Time
	refused int64
	tripped bool
}

// New wraps an Asker with spend and rate ceilings.
func New(inner Asker, opts Options) (*Limiter, error) {
	if inner == nil {
		return nil, errors.New("budget: an Asker is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxSpendUSD <= 0 {
		log.Warn("budget: no spend ceiling configured — a loop bug or a forgotten server can run up an unbounded bill")
	}
	if opts.MaxCallsPerMin <= 0 {
		log.Warn("budget: no rate ceiling configured — nothing will stop a runaway publish loop quickly")
	}
	return &Limiter{inner: inner, opts: opts, log: log}, nil
}

// Stats reports consumption so far.
type Stats struct {
	Calls    int64
	Tokens   int64
	SpentUSD float64
	Refused  int64
	Tripped  bool
}

// Stats returns a snapshot.
func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{
		Calls:    l.calls,
		Tokens:   l.tokens,
		SpentUSD: l.spentUSD,
		Refused:  l.refused,
		Tripped:  l.tripped,
	}
}

// Remaining reports how much of the spend ceiling is left. It returns -1
// when no ceiling is configured.
func (l *Limiter) Remaining() float64 {
	if l.opts.MaxSpendUSD <= 0 {
		return -1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.opts.MaxSpendUSD - l.spentUSD
	if r < 0 {
		return 0
	}
	return r
}

// Ask checks both ceilings, forwards the call, then records what it cost.
//
// The spend check uses what has already been spent, so a call that
// crosses the ceiling is allowed to finish and the ceiling is enforced
// from the next one. Overshoot is bounded by a single request, which is
// a fraction of a cent — far cheaper than the alternative of estimating
// cost up front and being wrong in the other direction.
func (l *Limiter) Ask(ctx context.Context, req jev.Request) (*jev.Response, error) {
	if err := l.reserve(); err != nil {
		return nil, err
	}

	resp, err := l.inner.Ask(ctx, req)
	if err != nil {
		// A failed call cost nothing to record, but it did consume a rate
		// slot — which is deliberate, since a retry storm is exactly what
		// the rate ceiling exists to stop.
		return nil, err
	}

	l.mu.Lock()
	l.calls++
	l.tokens += int64(resp.Usage.InputTokens)
	l.spentUSD += resp.Usage.Cost()
	spent, ceiling := l.spentUSD, l.opts.MaxSpendUSD
	l.mu.Unlock()

	if ceiling > 0 && spent >= ceiling*0.8 && spent < ceiling {
		l.log.Warn("budget: approaching spend ceiling",
			"spentUSD", fmt.Sprintf("%.6f", spent),
			"ceilingUSD", fmt.Sprintf("%.4f", ceiling))
	}
	return resp, nil
}

// reserve enforces both ceilings and claims a rate slot.
func (l *Limiter) reserve() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.opts.MaxSpendUSD > 0 && l.spentUSD >= l.opts.MaxSpendUSD {
		l.refused++
		if !l.tripped {
			l.tripped = true
			l.log.Error("budget: spend ceiling reached — refusing further judge calls; routing falls back to topic delivery",
				"spentUSD", fmt.Sprintf("%.6f", l.spentUSD),
				"ceilingUSD", fmt.Sprintf("%.4f", l.opts.MaxSpendUSD))
		}
		return fmt.Errorf("%w: spent $%.6f of $%.4f", ErrSpendExceeded, l.spentUSD, l.opts.MaxSpendUSD)
	}

	if l.opts.MaxCallsPerMin > 0 {
		cutoff := now().Add(-time.Minute)
		keep := l.recent[:0]
		for _, t := range l.recent {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		l.recent = keep

		if len(l.recent) >= l.opts.MaxCallsPerMin {
			l.refused++
			l.log.Error("budget: call rate ceiling reached — refusing judge call; routing falls back to topic delivery",
				"callsInLastMinute", len(l.recent),
				"ceiling", l.opts.MaxCallsPerMin)
			return fmt.Errorf("%w: %d calls in the last minute, limit %d",
				ErrRateExceeded, len(l.recent), l.opts.MaxCallsPerMin)
		}
		l.recent = append(l.recent, now())
	}
	return nil
}

// now is a seam for tests.
var now = time.Now
