package judge

import (
	"encoding/json"
	"testing"
)

// A publish nobody was listening for still happened. Recording it with no
// decisions is what lets a console tell "nobody was listening" apart from
// "nobody matched" — and from the publish having failed outright.
func TestAddPublishRecordsWithNoDecisions(t *testing.T) {
	r := NewRecorder(10)
	r.AddPublish("quiet.topic", json.RawMessage(`{"text":"hello"}`))

	got := r.Recent(10)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].Topic != "quiet.topic" {
		t.Fatalf("topic = %q", got[0].Topic)
	}
	if got[0].Decisions != nil {
		t.Fatalf("decisions = %v, want nil so it reads as unjudged", got[0].Decisions)
	}
	if got[0].At.IsZero() {
		t.Fatal("timestamp not set")
	}
}

func TestRecorderRingEvictsOldest(t *testing.T) {
	r := NewRecorder(3)
	for i := 0; i < 5; i++ {
		r.AddPublish("t", json.RawMessage(`{}`))
	}
	got := r.Recent(10)
	if len(got) != 3 {
		t.Fatalf("got %d records, want the ring capped at 3", len(got))
	}
	// Newest last, and sequence numbers keep counting past eviction.
	if got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("seqs = %d..%d, want 3..5", got[0].Seq, got[2].Seq)
	}
}

func TestSubscribeReceivesNewRecords(t *testing.T) {
	r := NewRecorder(5)
	ch, release := r.Subscribe()
	defer release()

	r.AddPublish("t", json.RawMessage(`{"n":1}`))
	select {
	case rec := <-ch:
		if rec.Topic != "t" {
			t.Fatalf("topic = %q", rec.Topic)
		}
	default:
		t.Fatal("subscriber received nothing")
	}
}

// A nil Recorder is the zero configuration and must be safe: the judge
// holds one whether or not a console is running.
func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.AddPublish("t", nil)
	if got := r.Recent(5); got != nil {
		t.Fatalf("Recent = %v, want nil", got)
	}
	ch, release := r.Subscribe()
	release()
	if _, open := <-ch; open {
		t.Fatal("want a closed channel from a nil recorder")
	}
}
