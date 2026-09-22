package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/interest"
)

// seedSubscriber is one demonstration interest.
type seedSubscriber struct {
	ID       string
	Criteria string
}

// demoSeeds give each default topic two subscribers whose interests
// overlap enough to be interesting.
//
// They are deliberately not cleanly separable: on alerts.infra a failing
// database volume is a storage problem and arguably an on-call problem,
// so the two disagree in the middle of the range rather than always
// splitting neatly. A seed set where every message matches exactly one
// subscriber would demonstrate string matching just as well.
var demoSeeds = map[string][]seedSubscriber{
	"alerts.infra": {
		{"dba", "database and storage problems, including disk capacity and replication lag"},
		{"oncall", "anything an on-call engineer would need to act on immediately"},
	},
	"deploys": {
		{"release", "rollbacks, failed deploys and anything that did not finish cleanly"},
		{"schema", "database migrations and schema changes"},
	},
	"security": {
		{"secops", "authentication failures, intrusion attempts and suspicious access patterns"},
		{"compliance", "changes to permissions, roles or audit configuration"},
	},
}

// seedDemoSubscribers registers the demonstration interests for any
// configured topic that currently has none.
//
// Only empty topics are touched, so this never overwrites or duplicates
// what someone has set up, and a topic whose subscribers were deleted on
// purpose stays empty for the life of the process. Failures are logged
// and ignored: a server that will not start because a demo fixture could
// not be written would be a poor trade.
func seedDemoSubscribers(ctx context.Context, store *interest.Store, topics []string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var added int
	for _, topic := range topics {
		seeds, ok := demoSeeds[topic]
		if !ok {
			continue
		}
		existing, err := store.Candidates(ctx, topic)
		if err != nil {
			log.Warn("seed: could not read interests", "topic", topic, "err", err.Error())
			continue
		}
		if len(existing) > 0 {
			continue
		}
		for _, s := range seeds {
			if err := store.Set(ctx, topic, s.ID, s.Criteria); err != nil {
				log.Warn("seed: could not write interest", "topic", topic, "id", s.ID, "err", err.Error())
				continue
			}
			added++
		}
	}
	if added > 0 {
		log.Info("seeded demonstration subscribers", "count", added)
	}
}
