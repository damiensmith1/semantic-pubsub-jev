// Package judge turns a published message and a set of stated interests
// into per-subscriber delivery decisions.
//
// It implements bus.Judge, so the broker calls it once per publish, on
// the publishing instance, before the message reaches the stream. Every
// candidate is evaluated in a single Jev request: latency is near-flat in
// candidate count while cost is linear, so batching is what makes this
// affordable at all.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/damiensmith1/go-ws-server/bus"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
)

// DefaultThreshold is the probability at or above which an interest counts
// as a match.
//
// 0.5 is a starting point, not a tuned value — M3 exists to find out
// whether a single global threshold is even the right dial. The raw
// probability is preserved on every Decision so thresholds can be
// re-examined without re-running the judge.
const DefaultThreshold = 0.5

// Asker is the part of the Jev client this package needs. Narrow on
// purpose: tests substitute a stub without a network or an API key.
type Asker interface {
	Ask(ctx context.Context, req jev.Request) (*jev.Response, error)
}

// Options configures a Judge.
type Options struct {
	// Threshold defaults to DefaultThreshold.
	Threshold float64

	// Log is optional.
	Log *slog.Logger
}

// Judge decides delivery using Jev.
type Judge struct {
	asker     Asker
	threshold float64
	log       *slog.Logger

	// Running totals, so cost is attributable rather than a surprise on
	// the invoice. Atomic because the broker judges publishes
	// concurrently.
	calls       atomic.Int64
	candidates  atomic.Int64
	inputTokens atomic.Int64
}

// New builds a Judge.
func New(asker Asker, opts Options) (*Judge, error) {
	if asker == nil {
		return nil, fmt.Errorf("judge: an Asker is required")
	}
	j := &Judge{asker: asker, threshold: opts.Threshold, log: opts.Log}
	if j.threshold <= 0 {
		j.threshold = DefaultThreshold
	}
	if j.log == nil {
		j.log = slog.Default()
	}
	return j, nil
}

// Stats reports what judging has cost so far.
type Stats struct {
	Calls       int64
	Candidates  int64
	InputTokens int64
}

// Cost estimates spend at the published input rate.
func (s Stats) Cost() float64 { return jev.Usage{InputTokens: int(s.InputTokens)}.Cost() }

// Stats returns a snapshot of cumulative usage.
func (j *Judge) Stats() Stats {
	return Stats{
		Calls:       j.calls.Load(),
		Candidates:  j.candidates.Load(),
		InputTokens: j.inputTokens.Load(),
	}
}

// Judge implements bus.Judge.
//
// Returning an error hands control to the broker's failure policy, which
// defaults to delivering by exact-topic match. That is deliberate: a
// broker that silently stops delivering when its classifier is
// unavailable fails worse than one that briefly over-delivers.
func (j *Judge) Judge(ctx context.Context, msg bus.Message, candidates []bus.Candidate) ([]bus.Decision, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	questions := make(map[string]jev.Noul, len(candidates))
	for _, c := range candidates {
		if c.Criteria == "" {
			// An empty predicate has no meaning to answer and would cost
			// tokens to ask. Omitted here, and therefore not delivered to.
			continue
		}
		questions[c.ID] = QuestionFor(c.Criteria)
	}
	if len(questions) == 0 {
		return nil, nil
	}

	resp, err := j.asker.Ask(ctx, jev.Request{
		State:     state(msg),
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}

	j.calls.Add(1)
	j.candidates.Add(int64(len(questions)))
	j.inputTokens.Add(int64(resp.Usage.InputTokens))

	decisions := make([]bus.Decision, 0, len(questions))
	var unanswered int
	for id := range questions {
		p, ok := resp.Probabilities[id]
		if !ok {
			// No answer is not a "no". It is a gap, and delivering on a
			// gap would be guessing, so the subscriber is skipped and the
			// gap is logged rather than silently becoming a decision.
			unanswered++
			continue
		}
		decisions = append(decisions, bus.Decision{
			ID:      id,
			Deliver: p >= j.threshold,
			Score:   p,
		})
	}

	if unanswered > 0 {
		j.log.Warn("judge: some candidates went unanswered",
			"topic", msg.Topic, "unanswered", unanswered, "asked", len(questions))
	}
	j.log.Debug("judge: decided",
		"topic", msg.Topic,
		"candidates", len(questions),
		"delivered", countDelivered(decisions),
		"inputTokens", resp.Usage.InputTokens,
		"latency", resp.Latency.String(),
		"model", resp.Model)

	return decisions, nil
}

func countDelivered(ds []bus.Decision) int {
	n := 0
	for _, d := range ds {
		if d.Deliver {
			n++
		}
	}
	return n
}

// QuestionFor renders one subscriber's interest as a Noul.
//
// Exported so the measurement harness asks exactly what the routing path
// asks. A harness with its own phrasing would measure a prompt that
// nothing in production uses.
//
// The phrasing asks about *delivery* rather than topical similarity. "Is
// this message about X?" and "would a subscriber interested in X want
// this?" diverge on the cases that matter: a subscriber watching for
// outages does not want a routine deploy notice that merely mentions the
// same service.
//
// The predicate is interpolated rather than concatenated loosely so the
// model sees a clear boundary between the instruction and the
// subscriber-supplied text. M3 measures how much this phrasing matters.
func QuestionFor(predicate string) jev.Noul {
	return jev.Noul{
		Instructions: "A subscriber has stated this interest: \"" + strings.TrimSpace(predicate) +
			"\". Should the message in the state be delivered to them?",
		True: "The message matches the stated interest and the subscriber would want to receive it.",
		False: "The message does not match the stated interest, or is only incidentally related, " +
			"and the subscriber would not want to receive it.",
	}
}

// StateFor presents a message to the model. Exported for the same reason
// as QuestionFor.
//
// The payload is decoded when it is valid JSON so the model sees named
// fields rather than an escaped string, which reads better and matches
// how Jev documents state. Invalid JSON is passed through as raw text
// rather than failing the publish.
func StateFor(topic string, body any) any {
	return map[string]any{"topic": topic, "message": body}
}

// state decodes a published payload and presents it.
func state(msg bus.Message) any {
	var decoded any
	if len(msg.Data) > 0 && json.Unmarshal(msg.Data, &decoded) == nil {
		return StateFor(msg.Topic, decoded)
	}
	return StateFor(msg.Topic, string(msg.Data))
}

// Judge must satisfy the broker's routing hook.
var _ bus.Judge = (*Judge)(nil)
