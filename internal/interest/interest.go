// Package interest stores each subscriber's stated interest, cluster-wide.
//
// The shape is a key per interest plus an index set per topic:
//
//	int:<topic>:<connID>  -> predicate, with a TTL
//	idx:<topic>           -> set of connIDs subscribed to that topic
//	conn:<connID>         -> set of topics that connection has interests in
//
// A hash per topic would read in one round trip instead of two, which is
// tempting because Candidates runs on every publish. It was rejected
// because Redis before 7.4 has no per-field TTL, so an instance killed
// without running its disconnect hook would leak its subscribers' fields
// permanently — and every later publish would pay to judge subscribers
// that no longer exist.
//
// The extra round trip costs well under a millisecond against a judge
// call measured at 200–530ms, so it is roughly half a percent of the
// publish budget. Self-healing is worth far more than that.
//
// conn:<connID> exists so a disconnect can find what to delete without
// scanning. Deletion is the normal path; the TTL is the backstop for when
// the normal path never runs.
package interest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/damiensmith1/go-ws-server/bus"
	"github.com/redis/go-redis/v9"
)

// DefaultTTL is how long an interest survives without a refresh. It is a
// backstop against instances that die without cleaning up, not the
// primary lifetime, so it is generous: too short and a quiet but healthy
// subscriber silently stops receiving anything.
const DefaultTTL = time.Hour

// Options configures a Store.
type Options struct {
	// TTL defaults to DefaultTTL.
	TTL time.Duration

	// KeyPrefix namespaces every key, so several deployments can share a
	// Redis without colliding. Empty means no prefix.
	KeyPrefix string
}

// Store is the cluster-wide interest registry. It is safe for concurrent
// use; the underlying client is.
type Store struct {
	rdb    redis.UniversalClient
	ttl    time.Duration
	prefix string
}

// New builds a Store.
func New(rdb redis.UniversalClient, opts Options) (*Store, error) {
	if rdb == nil {
		return nil, errors.New("interest: a redis client is required")
	}
	s := &Store{rdb: rdb, ttl: opts.TTL, prefix: opts.KeyPrefix}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	return s, nil
}

func (s *Store) interestKey(topic, connID string) string {
	return s.prefix + "int:" + topic + ":" + connID
}
func (s *Store) indexKey(topic string) string { return s.prefix + "idx:" + topic }
func (s *Store) connKey(connID string) string { return s.prefix + "conn:" + connID }

// Set records a connection's interest in a topic, replacing any previous
// one. An empty predicate is rejected: "no interest" is expressed by not
// registering, not by registering nothing, and an empty question would
// cost tokens to answer meaninglessly.
func (s *Store) Set(ctx context.Context, topic, connID, predicate string) error {
	if topic == "" || connID == "" {
		return errors.New("interest: topic and connID are required")
	}
	if predicate == "" {
		return errors.New("interest: predicate must not be empty")
	}

	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.interestKey(topic, connID), predicate, s.ttl)
	pipe.SAdd(ctx, s.indexKey(topic), connID)
	pipe.SAdd(ctx, s.connKey(connID), topic)
	// The index sets carry a TTL too, so a topic nobody has touched in a
	// long time does not persist as an empty set forever.
	pipe.Expire(ctx, s.indexKey(topic), s.ttl)
	pipe.Expire(ctx, s.connKey(connID), s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("interest: set %q for %q: %w", topic, connID, err)
	}
	return nil
}

// Candidates implements bus.CandidateSource.
//
// Entries whose interest key has expired are skipped and pruned from the
// index as they are found, so the index converges on the truth without a
// sweeper. Pruning failures are ignored: a stale index entry costs one
// wasted MGET slot next time, which is not worth failing a publish over.
func (s *Store) Candidates(ctx context.Context, topic string) ([]bus.Candidate, error) {
	connIDs, err := s.rdb.SMembers(ctx, s.indexKey(topic)).Result()
	if err != nil {
		return nil, fmt.Errorf("interest: read index for %q: %w", topic, err)
	}
	if len(connIDs) == 0 {
		return nil, nil
	}

	keys := make([]string, len(connIDs))
	for i, id := range connIDs {
		keys[i] = s.interestKey(topic, id)
	}
	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("interest: read interests for %q: %w", topic, err)
	}

	out := make([]bus.Candidate, 0, len(values))
	var stale []string
	for i, v := range values {
		predicate, ok := v.(string)
		if !ok || predicate == "" {
			stale = append(stale, connIDs[i])
			continue
		}
		out = append(out, bus.Candidate{ID: connIDs[i], Criteria: predicate})
	}

	if len(stale) > 0 {
		members := make([]any, len(stale))
		for i, id := range stale {
			members[i] = id
		}
		_ = s.rdb.SRem(ctx, s.indexKey(topic), members...).Err()
	}
	return out, nil
}

// Drop removes every interest held by a connection. This is the normal
// cleanup path, driven by the server's disconnect hook.
func (s *Store) Drop(ctx context.Context, connID string) error {
	if connID == "" {
		return nil
	}
	topics, err := s.rdb.SMembers(ctx, s.connKey(connID)).Result()
	if err != nil {
		return fmt.Errorf("interest: read topics for %q: %w", connID, err)
	}

	pipe := s.rdb.TxPipeline()
	for _, topic := range topics {
		pipe.Del(ctx, s.interestKey(topic, connID))
		pipe.SRem(ctx, s.indexKey(topic), connID)
	}
	pipe.Del(ctx, s.connKey(connID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("interest: drop %q: %w", connID, err)
	}
	return nil
}

// Refresh extends the TTL on a connection's interests.
//
// Called while a connection is demonstrably alive — from middleware on
// inbound frames — so that the TTL only ever expires interests belonging
// to connections that have genuinely gone away.
func (s *Store) Refresh(ctx context.Context, connID string) error {
	if connID == "" {
		return nil
	}
	topics, err := s.rdb.SMembers(ctx, s.connKey(connID)).Result()
	if err != nil {
		return fmt.Errorf("interest: read topics for %q: %w", connID, err)
	}
	if len(topics) == 0 {
		return nil
	}

	pipe := s.rdb.Pipeline()
	for _, topic := range topics {
		pipe.Expire(ctx, s.interestKey(topic, connID), s.ttl)
		pipe.Expire(ctx, s.indexKey(topic), s.ttl)
	}
	pipe.Expire(ctx, s.connKey(connID), s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("interest: refresh %q: %w", connID, err)
	}
	return nil
}

// Store must satisfy the server's CandidateSource, or it cannot be wired
// in as the judge's source of subscribers.
var _ bus.CandidateSource = (*Store)(nil)
