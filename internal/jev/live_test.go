//go:build live

// Live tests call the real API and cost money. They are excluded from the
// default build and run only when asked for:
//
//	go test -tags=live ./internal/jev/ -v
//
// They exist because the stub tests prove only that the client is
// consistent with this package's own assumptions about the wire format.
// Only a real call proves the assumptions are right.
package jev

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// loadKey reads TYPESAFE_API_KEY from the environment, falling back to a
// .env file so a live run needs no shell setup.
func loadKey(t *testing.T) string {
	t.Helper()
	if k := os.Getenv("TYPESAFE_API_KEY"); k != "" {
		return k
	}
	for _, path := range []string{".env", "../../.env"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if ok && strings.TrimSpace(k) == "TYPESAFE_API_KEY" {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					return v
				}
			}
		}
	}
	t.Skip("no TYPESAFE_API_KEY in environment or .env; skipping live test")
	return ""
}

// TestLiveDiscriminates is the one assertion that matters for routing: a
// message about disk pressure must score high for a subscriber interested
// in storage and low for one interested in something else. If that
// separation does not hold, nothing downstream can work.
func TestLiveDiscriminates(t *testing.T) {
	c, err := New(Options{APIKey: loadKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	interests := map[string]string{
		"storage":  "database and storage problems, including disk capacity",
		"network":  "network latency and connectivity issues",
		"security": "authentication failures and security incidents",
		"eu":       "anything affecting the eu-west region",
	}
	questions := make(map[string]Noul, len(interests))
	for id, predicate := range interests {
		questions[id] = Noul{
			Instructions: "Should this message be delivered to a subscriber whose stated interest is: " + predicate + "?",
			True:         "The message matches the stated interest and the subscriber would want to receive it.",
			False:        "The message does not match the stated interest.",
		}
	}

	resp, err := c.Ask(ctx, Request{
		State: map[string]any{
			"topic": "alerts.infra",
			"message": map[string]any{
				"service":  "orders-db",
				"severity": "critical",
				"text":     "primary database volume at 96% capacity, writes will fail within the hour",
				"region":   "us-east-1",
			},
		},
		Questions: questions,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	t.Logf("model=%s latency=%s input_tokens=%d cost=$%.6f",
		resp.Model, resp.Latency.Round(time.Millisecond), resp.Usage.InputTokens, resp.Usage.Cost())
	for id := range interests {
		t.Logf("  %-9s %.3f", id, resp.Probabilities[id])
	}

	if len(resp.Probabilities) != len(interests) {
		t.Fatalf("got %d answers, want %d", len(resp.Probabilities), len(interests))
	}
	if resp.Model == "" {
		t.Error("response did not report the answering model")
	}
	if resp.Usage.InputTokens == 0 {
		t.Error("usage reported no input tokens; cost tracking would be blind")
	}

	// The separation, not the absolute values. Thresholds are M3's job.
	if resp.Probabilities["storage"] <= resp.Probabilities["network"] {
		t.Errorf("storage %.3f should outrank network %.3f for a disk-capacity alert",
			resp.Probabilities["storage"], resp.Probabilities["network"])
	}
	if resp.Probabilities["storage"] <= resp.Probabilities["security"] {
		t.Errorf("storage %.3f should outrank security %.3f",
			resp.Probabilities["storage"], resp.Probabilities["security"])
	}
	// The message says us-east-1, so a subscriber watching eu-west should
	// not match. This catches a judge keying on topic rather than content.
	if resp.Probabilities["eu"] >= resp.Probabilities["storage"] {
		t.Errorf("eu %.3f should not outrank storage %.3f for a us-east-1 alert",
			resp.Probabilities["eu"], resp.Probabilities["storage"])
	}
}
