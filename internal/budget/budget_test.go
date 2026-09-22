package budget

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// stubAsker reports a fixed token cost per call.
type stubAsker struct {
	mu     sync.Mutex
	tokens int
	calls  int
	err    error
}

func (s *stubAsker) Ask(context.Context, jev.Request) (*jev.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return &jev.Response{Usage: jev.Usage{InputTokens: s.tokens}}, nil
}

func (s *stubAsker) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newLimiter(t *testing.T, inner Asker, o Options) *Limiter {
	t.Helper()
	o.Log = quiet()
	l, err := New(inner, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// fakeClock lets the rate window be tested without sleeping.
func fakeClock(t *testing.T) func(time.Duration) {
	t.Helper()
	base := time.Now()
	cur := base
	orig := now
	now = func() time.Time { return cur }
	t.Cleanup(func() { now = orig })
	return func(d time.Duration) { cur = cur.Add(d) }
}

func TestNewRequiresAsker(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("want an error without an Asker")
	}
}

// 1_000_000 tokens is exactly $0.042.
func TestSpendCeilingStopsFurtherCalls(t *testing.T) {
	s := &stubAsker{tokens: 1_000_000}
	l := newLimiter(t, s, Options{MaxSpendUSD: 0.10})

	// Three calls take spend to $0.126, past the $0.10 ceiling.
	for i := 0; i < 3; i++ {
		if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	_, err := l.Ask(context.Background(), jev.Request{})
	if !errors.Is(err, ErrSpendExceeded) {
		t.Fatalf("err = %v, want ErrSpendExceeded", err)
	}
	if s.count() != 3 {
		t.Fatalf("forwarded %d calls, want 3 — the refusal must not reach the API", s.count())
	}

	st := l.Stats()
	if !st.Tripped || st.Refused != 1 {
		t.Fatalf("stats = %+v, want tripped with one refusal", st)
	}
	if l.Remaining() != 0 {
		t.Fatalf("Remaining() = %v, want 0", l.Remaining())
	}
}

// Overshoot is bounded to a single request: the ceiling is enforced from
// the call after the one that crossed it.
func TestOvershootIsOneCall(t *testing.T) {
	s := &stubAsker{tokens: 10_000_000} // $0.42 in one go
	l := newLimiter(t, s, Options{MaxSpendUSD: 0.01})

	if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
		t.Fatalf("first call should be allowed: %v", err)
	}
	if _, err := l.Ask(context.Background(), jev.Request{}); !errors.Is(err, ErrSpendExceeded) {
		t.Fatalf("err = %v, want ErrSpendExceeded", err)
	}
	if s.count() != 1 {
		t.Fatalf("forwarded %d calls, want 1", s.count())
	}
}

// The guard that actually stops a loop bug.
func TestRateCeiling(t *testing.T) {
	advance := fakeClock(t)
	s := &stubAsker{tokens: 1}
	l := newLimiter(t, s, Options{MaxCallsPerMin: 5})

	for i := 0; i < 5; i++ {
		if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := l.Ask(context.Background(), jev.Request{}); !errors.Is(err, ErrRateExceeded) {
		t.Fatalf("err = %v, want ErrRateExceeded", err)
	}
	if s.count() != 5 {
		t.Fatalf("forwarded %d calls, want 5", s.count())
	}

	// The window slides: once a minute passes the budget is available again.
	advance(61 * time.Second)
	if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
		t.Fatalf("after the window slid: %v", err)
	}
}

// A retry storm is exactly what the rate ceiling exists to stop, so a
// failed call must still consume a slot.
func TestFailedCallsConsumeRateBudget(t *testing.T) {
	fakeClock(t)
	s := &stubAsker{tokens: 1, err: errors.New("api down")}
	l := newLimiter(t, s, Options{MaxCallsPerMin: 3})

	for i := 0; i < 3; i++ {
		if _, err := l.Ask(context.Background(), jev.Request{}); err == nil {
			t.Fatal("want the inner error")
		}
	}
	if _, err := l.Ask(context.Background(), jev.Request{}); !errors.Is(err, ErrRateExceeded) {
		t.Fatalf("err = %v, want the rate ceiling to have engaged", err)
	}
}

// A failed call cost nothing, so it must not count toward spend.
func TestFailedCallsDoNotCountTowardSpend(t *testing.T) {
	s := &stubAsker{tokens: 1_000_000, err: errors.New("api down")}
	l := newLimiter(t, s, Options{MaxSpendUSD: 0.01})

	_, _ = l.Ask(context.Background(), jev.Request{})
	if st := l.Stats(); st.SpentUSD != 0 || st.Calls != 0 {
		t.Fatalf("stats = %+v, want nothing recorded for a failed call", st)
	}
}

func TestZeroLimitsDisableChecks(t *testing.T) {
	s := &stubAsker{tokens: 1_000_000}
	l := newLimiter(t, s, Options{}) // both ceilings off

	for i := 0; i < 20; i++ {
		if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
			t.Fatalf("call %d refused with no ceilings configured: %v", i, err)
		}
	}
	if l.Remaining() != -1 {
		t.Fatalf("Remaining() = %v, want -1 when no ceiling is set", l.Remaining())
	}
}

func TestStatsAccumulate(t *testing.T) {
	s := &stubAsker{tokens: 500_000}
	l := newLimiter(t, s, Options{MaxSpendUSD: 1})

	for i := 0; i < 4; i++ {
		if _, err := l.Ask(context.Background(), jev.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	st := l.Stats()
	if st.Calls != 4 || st.Tokens != 2_000_000 {
		t.Fatalf("stats = %+v, want 4 calls / 2M tokens", st)
	}
	if want := 2_000_000.0 / 1e6 * 0.042; st.SpentUSD != want {
		t.Fatalf("SpentUSD = %v, want %v", st.SpentUSD, want)
	}
}

func TestConcurrentAsksAreSafe(t *testing.T) {
	s := &stubAsker{tokens: 100}
	l := newLimiter(t, s, Options{MaxSpendUSD: 10, MaxCallsPerMin: 1000})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Ask(context.Background(), jev.Request{})
		}()
	}
	wg.Wait()

	if st := l.Stats(); st.Calls != 50 {
		t.Fatalf("Calls = %d, want 50", st.Calls)
	}
}

var _ Asker = (*Limiter)(nil)
