package judge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/damiensmith1/go-ws-server/bus"
)

func TestKeywordRoutes(t *testing.T) {
	k := Keyword{}
	m := bus.Message{Topic: "alerts", Data: json.RawMessage(
		`{"text":"primary database volume at 96% capacity"}`)}

	ds, err := k.Judge(context.Background(), m, []bus.Candidate{
		{ID: "storage", Criteria: "database and storage problems"},
		{ID: "network", Criteria: "network latency issues"},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	got := decisionsByID(ds)
	if !got["storage"].Deliver {
		t.Errorf("storage should match on 'database': %+v", got["storage"])
	}
	if got["network"].Deliver {
		t.Errorf("network should not match: %+v", got["network"])
	}
	if got["storage"].Score <= 0 || got["storage"].Score > 1 {
		t.Errorf("Score = %v, want a 0..1 fraction", got["storage"].Score)
	}
}

// Short words match almost anything and would make every predicate hit.
func TestKeywordIgnoresShortWords(t *testing.T) {
	k := Keyword{}
	m := bus.Message{Data: json.RawMessage(`{"text":"an is at to the"}`)}

	ds, _ := k.Judge(context.Background(), m, []bus.Candidate{
		{ID: "noise", Criteria: "an is at to the"},
	})
	if len(ds) != 1 || ds[0].Deliver {
		t.Fatalf("decisions = %+v, want no delivery from stopword-only overlap", ds)
	}
}

func TestKeywordSkipsEmptyCriteria(t *testing.T) {
	ds, _ := Keyword{}.Judge(context.Background(),
		bus.Message{Data: json.RawMessage(`{"a":1}`)},
		[]bus.Candidate{{ID: "blank", Criteria: ""}})
	if len(ds) != 0 {
		t.Fatalf("decisions = %+v, want none", ds)
	}
}
