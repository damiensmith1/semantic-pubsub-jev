package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/damiensmith1/go-ws-server/bus"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// stubAsker answers from a fixed table, and records what it was asked.
type stubAsker struct {
	probs  map[string]float64
	tokens int
	err    error
	got    jev.Request
	calls  int
}

func (s *stubAsker) Ask(_ context.Context, req jev.Request) (*jev.Response, error) {
	s.calls++
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]float64{}
	for id := range req.Questions {
		if p, ok := s.probs[id]; ok {
			out[id] = p
		}
	}
	return &jev.Response{
		Model:         "jev-test",
		Probabilities: out,
		Usage:         jev.Usage{InputTokens: s.tokens},
	}, nil
}

func newJudge(t *testing.T, s *stubAsker, threshold float64) *Judge {
	t.Helper()
	j, err := New(s, Options{Threshold: threshold, Log: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return j
}

func msg() bus.Message {
	return bus.Message{Topic: "alerts", Data: json.RawMessage(`{"severity":"critical"}`)}
}

func decisionsByID(ds []bus.Decision) map[string]bus.Decision {
	m := make(map[string]bus.Decision, len(ds))
	for _, d := range ds {
		m[d.ID] = d
	}
	return m
}

func TestNewRequiresAsker(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("want an error without an Asker")
	}
}

func TestThresholdDecidesDelivery(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{
		"high":  0.98,
		"edge":  0.60, // exactly the threshold: inclusive
		"below": 0.59,
		"low":   0.02,
	}, tokens: 100}
	j := newJudge(t, s, 0.60)

	ds, err := j.Judge(context.Background(), msg(), []bus.Candidate{
		{ID: "high", Criteria: "a"}, {ID: "edge", Criteria: "b"},
		{ID: "below", Criteria: "c"}, {ID: "low", Criteria: "d"},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	got := decisionsByID(ds)
	for id, want := range map[string]bool{"high": true, "edge": true, "below": false, "low": false} {
		if got[id].Deliver != want {
			t.Errorf("%s: Deliver = %v (score %.2f), want %v", id, got[id].Deliver, got[id].Score, want)
		}
	}

	// The raw probability must survive, or M3 cannot re-examine a
	// threshold without paying to re-run the judge.
	if got["high"].Score != 0.98 {
		t.Errorf("Score = %v, want the raw probability preserved", got["high"].Score)
	}
}

// One request for all candidates is the whole economic argument.
func TestBatchesIntoOneRequest(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{}, tokens: 500}
	for i := range make([]struct{}, 50) {
		s.probs[string(rune('a'+i%26))+string(rune('0'+i/26))] = 0.9
	}
	j := newJudge(t, s, 0.5)

	var cands []bus.Candidate
	for id := range s.probs {
		cands = append(cands, bus.Candidate{ID: id, Criteria: "something"})
	}

	if _, err := j.Judge(context.Background(), msg(), cands); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if s.calls != 1 {
		t.Fatalf("made %d calls for %d candidates, want exactly 1", s.calls, len(cands))
	}
	if len(s.got.Questions) != len(cands) {
		t.Fatalf("asked %d questions for %d candidates", len(s.got.Questions), len(cands))
	}
}

// A gap in the answers is not a "no". Delivering, or refusing to deliver,
// on a missing answer would be guessing.
func TestUnansweredCandidateGetsNoDecision(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{"answered": 0.9}, tokens: 10}
	j := newJudge(t, s, 0.5)

	ds, err := j.Judge(context.Background(), msg(), []bus.Candidate{
		{ID: "answered", Criteria: "a"},
		{ID: "ignored", Criteria: "b"},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(ds) != 1 || ds[0].ID != "answered" {
		t.Fatalf("decisions = %+v, want only the answered candidate", ds)
	}
}

func TestEmptyCriteriaIsNotAsked(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{"real": 0.9}, tokens: 10}
	j := newJudge(t, s, 0.5)

	ds, err := j.Judge(context.Background(), msg(), []bus.Candidate{
		{ID: "real", Criteria: "storage"},
		{ID: "blank", Criteria: ""},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if _, asked := s.got.Questions["blank"]; asked {
		t.Error("an empty predicate was sent as a question, costing tokens for nothing")
	}
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v, want only the real candidate", ds)
	}
}

func TestNoCandidatesMakesNoCall(t *testing.T) {
	s := &stubAsker{}
	j := newJudge(t, s, 0.5)

	ds, err := j.Judge(context.Background(), msg(), nil)
	if err != nil || ds != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", ds, err)
	}
	if s.calls != 0 {
		t.Fatalf("made %d calls with no candidates, want 0", s.calls)
	}
}

// The error must reach the broker so its failure policy applies. Silently
// returning no decisions would look like "nobody matched" and drop the
// message for everyone.
func TestAskerErrorPropagates(t *testing.T) {
	s := &stubAsker{err: errors.New("classifier unavailable")}
	j := newJudge(t, s, 0.5)

	ds, err := j.Judge(context.Background(), msg(), []bus.Candidate{{ID: "a", Criteria: "x"}})
	if err == nil {
		t.Fatal("want the error to propagate to the broker's failure policy")
	}
	if ds != nil {
		t.Fatalf("decisions = %+v, want none alongside an error", ds)
	}
}

// Cost must be attributable, not a surprise on the invoice (N1).
func TestStatsAccumulate(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{"a": 0.9, "b": 0.1}, tokens: 250}
	j := newJudge(t, s, 0.5)
	cands := []bus.Candidate{{ID: "a", Criteria: "x"}, {ID: "b", Criteria: "y"}}

	for i := 0; i < 3; i++ {
		if _, err := j.Judge(context.Background(), msg(), cands); err != nil {
			t.Fatalf("Judge: %v", err)
		}
	}

	st := j.Stats()
	if st.Calls != 3 || st.Candidates != 6 || st.InputTokens != 750 {
		t.Fatalf("stats = %+v, want 3 calls / 6 candidates / 750 tokens", st)
	}
	if want := 750.0 / 1e6 * 0.042; st.Cost() != want {
		t.Fatalf("Cost() = %v, want %v", st.Cost(), want)
	}
}

// A failed call must not be counted as spend.
func TestFailedCallIsNotCounted(t *testing.T) {
	s := &stubAsker{err: errors.New("boom")}
	j := newJudge(t, s, 0.5)
	_, _ = j.Judge(context.Background(), msg(), []bus.Candidate{{ID: "a", Criteria: "x"}})

	if st := j.Stats(); st.Calls != 0 || st.InputTokens != 0 {
		t.Fatalf("stats = %+v, want nothing counted for a failed call", st)
	}
}

// JSON payloads should reach the model as named fields, not an escaped
// string — that is how Jev documents state, and it reads better.
func TestStateDecodesJSONPayload(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{"a": 0.9}, tokens: 10}
	j := newJudge(t, s, 0.5)

	_, err := j.Judge(context.Background(),
		bus.Message{Topic: "alerts", Data: json.RawMessage(`{"service":"db","disk":96}`)},
		[]bus.Candidate{{ID: "a", Criteria: "storage"}})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	st, ok := s.got.State.(map[string]any)
	if !ok {
		t.Fatalf("State = %T, want a map", s.got.State)
	}
	if st["topic"] != "alerts" {
		t.Errorf("topic = %v", st["topic"])
	}
	inner, ok := st["message"].(map[string]any)
	if !ok {
		t.Fatalf("message = %T, want decoded JSON not a string", st["message"])
	}
	if inner["service"] != "db" {
		t.Errorf("message.service = %v, want db", inner["service"])
	}
}

// Malformed payloads must not fail the publish.
func TestStateFallsBackToRawText(t *testing.T) {
	s := &stubAsker{probs: map[string]float64{"a": 0.9}, tokens: 10}
	j := newJudge(t, s, 0.5)

	_, err := j.Judge(context.Background(),
		bus.Message{Topic: "alerts", Data: json.RawMessage(`not json at all`)},
		[]bus.Candidate{{ID: "a", Criteria: "storage"}})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	st := s.got.State.(map[string]any)
	if _, isString := st["message"].(string); !isString {
		t.Fatalf("message = %T, want the raw text preserved", st["message"])
	}
}

func TestQuestionIncludesPredicateAndBothOutcomes(t *testing.T) {
	q := question("  database and storage problems  ")

	if !strings.Contains(q.Instructions, "database and storage problems") {
		t.Errorf("instructions do not carry the predicate: %q", q.Instructions)
	}
	if strings.Contains(q.Instructions, "  database") {
		t.Errorf("predicate was not trimmed: %q", q.Instructions)
	}
	// Both outcomes described; a one-sided question is less well calibrated.
	if q.True == "" || q.False == "" {
		t.Errorf("criteria = %q / %q, want both described", q.True, q.False)
	}
	// The question must ask about delivery, not topical similarity: a
	// subscriber watching for outages does not want a deploy notice that
	// merely mentions the same service.
	if !strings.Contains(q.Instructions, "delivered") {
		t.Errorf("instructions should ask about delivery: %q", q.Instructions)
	}
}
