package judge

import (
	"context"
	"strings"

	"github.com/damiensmith1/go-ws-server/bus"
)

// Keyword is a deterministic stand-in for the Jev judge: it delivers when
// any word of a subscriber's predicate appears in the message.
//
// It exists so the system can be run and tested end to end without
// spending tokens or depending on the network, and so integration tests
// assert on routing rather than on a model's judgment. It is not a
// fallback for production use — it has none of the semantic behaviour
// that is the point of the project, and it will happily deliver a deploy
// notice to someone watching for outages because both say "checkout".
type Keyword struct {
	// MinWordLen ignores short words, which otherwise match everything.
	// Zero means 4.
	MinWordLen int
}

// Judge implements bus.Judge.
func (k Keyword) Judge(_ context.Context, msg bus.Message, candidates []bus.Candidate) ([]bus.Decision, error) {
	minLen := k.MinWordLen
	if minLen <= 0 {
		minLen = 4
	}
	body := strings.ToLower(string(msg.Data))

	out := make([]bus.Decision, 0, len(candidates))
	for _, c := range candidates {
		if c.Criteria == "" {
			continue
		}
		var hits, words int
		for _, w := range strings.FieldsFunc(strings.ToLower(c.Criteria), func(r rune) bool {
			return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9')
		}) {
			if len(w) < minLen {
				continue
			}
			words++
			if strings.Contains(body, w) {
				hits++
			}
		}
		// Score is the fraction of the predicate's significant words that
		// appeared, so it occupies the same 0..1 range as a probability
		// and the same threshold logic applies.
		var score float64
		if words > 0 {
			score = float64(hits) / float64(words)
		}
		out = append(out, bus.Decision{ID: c.ID, Deliver: hits > 0, Score: score})
	}
	return out, nil
}

var _ bus.Judge = Keyword{}
