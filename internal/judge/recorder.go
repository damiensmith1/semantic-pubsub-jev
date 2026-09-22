package judge

import (
	"encoding/json"
	"sync"
	"time"
)

// Decision is one subscriber's outcome for one message, as the judge
// actually decided it.
type Decision struct {
	CandidateID string  `json:"id"`
	Criteria    string  `json:"criteria"`
	Score       float64 `json:"score"`
	Deliver     bool    `json:"deliver"`
}

// Record is everything the judge decided for one published message.
//
// It exists so a UI can show what routing actually did, rather than
// re-judging the same message to find out — which would double the cost
// and could disagree with the decision that was really applied.
type Record struct {
	Seq         int64           `json:"seq"`
	At          time.Time       `json:"at"`
	Topic       string          `json:"topic"`
	Data        json.RawMessage `json:"data"`
	Threshold   float64         `json:"threshold"`
	Decisions   []Decision      `json:"decisions"`
	InputTokens int             `json:"inputTokens"`
	LatencyMS   float64         `json:"latencyMs"`
	Model       string          `json:"model"`
}

// Recorder keeps the most recent judgments in memory.
//
// Bounded on purpose: this is an observation window for a live view, not
// storage. Anything that must outlive the process belongs in the raw
// JSONL the measurement harness writes.
type Recorder struct {
	mu   sync.RWMutex
	ring []Record
	next int
	seq  int64
	size int

	// subs are notified of each new record, for streaming views.
	subs map[int]chan Record
	subN int
}

// NewRecorder returns a Recorder holding at most size records.
func NewRecorder(size int) *Recorder {
	if size <= 0 {
		size = 100
	}
	return &Recorder{
		ring: make([]Record, 0, size),
		size: size,
		subs: map[int]chan Record{},
	}
}

func (r *Recorder) add(rec Record) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.seq++
	rec.Seq = r.seq
	if len(r.ring) < r.size {
		r.ring = append(r.ring, rec)
	} else {
		r.ring[r.next] = rec
		r.next = (r.next + 1) % r.size
	}
	subs := make([]chan Record, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()

	// Non-blocking: a stalled viewer must never hold up a publish.
	for _, ch := range subs {
		select {
		case ch <- rec:
		default:
		}
	}
}

// AddPublish records a message that was published with nothing to judge.
//
// The bus skips the judge entirely when a topic has no candidates, so
// without this a publish to a topic nobody subscribes to would leave no
// trace at all — and a console showing only judged messages would look
// like the publish had failed. A nil Decisions slice is what distinguishes
// "nobody was listening" from "nobody matched".
func (r *Recorder) AddPublish(topic string, data json.RawMessage) {
	if r == nil {
		return
	}
	r.add(Record{At: time.Now().UTC(), Topic: topic, Data: data})
}

// Recent returns up to n records, newest last.
func (r *Recorder) Recent(n int) []Record {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Record, 0, len(r.ring))
	if len(r.ring) < r.size {
		out = append(out, r.ring...)
	} else {
		out = append(out, r.ring[r.next:]...)
		out = append(out, r.ring[:r.next]...)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Subscribe returns a channel of new records and a function to release it.
func (r *Recorder) Subscribe() (<-chan Record, func()) {
	if r == nil {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan Record, 16)
	r.mu.Lock()
	id := r.subN
	r.subN++
	r.subs[id] = ch
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
		r.mu.Unlock()
	}
}
