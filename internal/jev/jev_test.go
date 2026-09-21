package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a Client at a stub server. No test in this file
// touches the real API, so the suite costs nothing and runs in CI.
func newTestClient(t *testing.T, h http.HandlerFunc, opts ...func(*Options)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	o := Options{APIKey: "test-key", BaseURL: srv.URL, MaxRetries: 2}
	for _, f := range opts {
		f(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func okHandler(answers map[string]float64, inputTokens int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"model":   "jev-1.13.0",
			"answers": map[string]any{},
			"usage":   map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		}
		as := out["answers"].(map[string]any)
		for id, p := range answers {
			as[id] = map[string]any{"type": "noul", "noul": p}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func twoQuestions() Request {
	return Request{
		State: map[string]any{"service": "checkout", "severity": "warning"},
		Questions: map[string]Noul{
			"conn-1": {Instructions: "storage problems?", True: "yes", False: "no"},
			"conn-2": {Instructions: "network problems?", True: "yes", False: "no"},
		},
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("want an error when APIKey is missing")
	}
	c, err := New(Options{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.model != DefaultModel || c.baseURL != DefaultBaseURL {
		t.Fatalf("defaults not applied: model=%q baseURL=%q", c.model, c.baseURL)
	}
}

func TestAskParsesAnswers(t *testing.T) {
	c := newTestClient(t, okHandler(map[string]float64{"conn-1": 0.96, "conn-2": 0.04}, 606))

	resp, err := c.Ask(context.Background(), twoQuestions())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if resp.Probabilities["conn-1"] != 0.96 || resp.Probabilities["conn-2"] != 0.04 {
		t.Fatalf("probabilities = %v", resp.Probabilities)
	}
	// The versioned ID that answered, not the alias asked for, so a run's
	// results stay attributable when the alias moves.
	if resp.Model != "jev-1.13.0" {
		t.Fatalf("Model = %q, want the versioned id", resp.Model)
	}
	if resp.Usage.InputTokens != 606 {
		t.Fatalf("InputTokens = %d, want 606", resp.Usage.InputTokens)
	}
	if resp.Latency <= 0 {
		t.Fatal("Latency was not recorded")
	}
}

// "No answer" and "certainly not" are different facts. Collapsing a
// missing answer to 0 would silently route as a confident no.
func TestMissingAnswerIsAbsentNotZero(t *testing.T) {
	c := newTestClient(t, okHandler(map[string]float64{"conn-1": 0.9}, 10))

	resp, err := c.Ask(context.Background(), twoQuestions())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, ok := resp.Probabilities["conn-2"]; ok {
		t.Fatal("an unanswered question appeared in the map")
	}
	if len(resp.Probabilities) != 1 {
		t.Fatalf("probabilities = %v, want only the answered one", resp.Probabilities)
	}
}

func TestRequestEncoding(t *testing.T) {
	var got wireRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		okHandler(map[string]float64{"conn-1": 1, "conn-2": 0}, 1)(w, r)
	})

	if _, err := c.Ask(context.Background(), twoQuestions()); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Model != DefaultModel {
		t.Fatalf("model = %q", got.Model)
	}
	q, ok := got.Questions["conn-1"]
	if !ok {
		t.Fatalf("question missing: %+v", got.Questions)
	}
	if q.Type != "noul" {
		t.Fatalf("type = %q, want noul", q.Type)
	}
	// Both outcomes must be described; a one-sided question is less well
	// calibrated.
	if q.Criteria["true"] == "" || q.Criteria["false"] == "" {
		t.Fatalf("criteria = %v, want both outcomes described", q.Criteria)
	}
}

func TestAskRejectsEmptyQuestions(t *testing.T) {
	c := newTestClient(t, okHandler(nil, 0))
	if _, err := c.Ask(context.Background(), Request{State: "x"}); err == nil {
		t.Fatal("want an error for a request with no questions")
	}
}

// A malformed or unauthorised request cannot succeed on retry; retrying
// it just burns budget and delays the error.
func TestNonRetryableErrorsFailImmediately(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			})

			_, err := c.Ask(context.Background(), twoQuestions())
			if err == nil {
				t.Fatal("want an error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want an *APIError, got %T", err)
			}
			if apiErr.StatusCode != code {
				t.Fatalf("StatusCode = %d, want %d", apiErr.StatusCode, code)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("made %d calls, want 1: %d must not be retried", n, code)
			}
		})
	}
}

func TestRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// The server knows when its limit resets; honour it rather
			// than guessing with exponential backoff.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		okHandler(map[string]float64{"conn-1": 0.5, "conn-2": 0.5}, 10)(w, r)
	})

	resp, err := c.Ask(context.Background(), twoQuestions())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("made %d calls, want 2 (one 429 then success)", n)
	}
	if len(resp.Probabilities) != 2 {
		t.Fatalf("probabilities = %v", resp.Probabilities)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}, func(o *Options) { o.MaxRetries = 2 })

	if _, err := c.Ask(context.Background(), twoQuestions()); err == nil {
		t.Fatal("want an error once retries are exhausted")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("made %d calls, want 3 (initial + 2 retries)", n)
	}
}

// A Judge sits on the publish path, so a hung call must not outlive its
// deadline.
func TestContextCancellationIsPrompt(t *testing.T) {
	// Bounded, not `<-r.Context().Done()`: httptest's cleanup waits for
	// outstanding handlers, so a handler that only exits on cancellation
	// can deadlock against the very Close that would cancel it.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Ask(ctx, twoQuestions())
	if err == nil {
		t.Fatal("want an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %s; the deadline did not apply", elapsed)
	}
}

// Retrying past the deadline would keep a doomed call alive.
func TestNoRetryAfterContextExpires(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Ask(ctx, twoQuestions()); err == nil {
		t.Fatal("want an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("waited %s honouring Retry-After past the deadline", elapsed)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d calls, want 1", n)
	}
}

func TestUsageCost(t *testing.T) {
	// $0.042 per Mtok of input; output is free.
	u := Usage{InputTokens: 1_000_000, OutputTokens: 999}
	if got := u.Cost(); got != 0.042 {
		t.Fatalf("Cost() = %v, want 0.042", got)
	}
	if got := (Usage{}).Cost(); got != 0 {
		t.Fatalf("Cost() = %v, want 0", got)
	}
}

func TestMalformedResponseBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})
	if _, err := c.Ask(context.Background(), twoQuestions()); err == nil {
		t.Fatal("want a decode error")
	}
}
