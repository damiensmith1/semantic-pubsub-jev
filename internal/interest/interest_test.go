package interest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T, opts Options) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s, err := New(rdb, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, mr
}

func ids(t *testing.T, s *Store, topic string) []string {
	t.Helper()
	cs, err := s.Candidates(context.Background(), topic)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	sort.Strings(out)
	return out
}

func TestNewRequiresClient(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("want an error without a redis client")
	}
}

func TestSetAndCandidates(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	if err := s.Set(ctx, "alerts", "conn-1", "storage problems"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(ctx, "alerts", "conn-2", "network problems"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cs, err := s.Candidates(ctx, "alerts")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d candidates, want 2", len(cs))
	}
	byID := map[string]string{}
	for _, c := range cs {
		byID[c.ID] = c.Criteria
	}
	if byID["conn-1"] != "storage problems" || byID["conn-2"] != "network problems" {
		t.Fatalf("predicates did not round trip: %v", byID)
	}
}

func TestCandidatesUnknownTopic(t *testing.T) {
	s, _ := newStore(t, Options{})
	cs, err := s.Candidates(context.Background(), "nobody-here")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cs) != 0 {
		t.Fatalf("got %d candidates, want none", len(cs))
	}
}

// One connection may care about different things on different topics.
func TestInterestsArePerTopic(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	_ = s.Set(ctx, "alerts", "conn-1", "storage")
	_ = s.Set(ctx, "deploys", "conn-1", "rollbacks")

	alerts, _ := s.Candidates(ctx, "alerts")
	deploys, _ := s.Candidates(ctx, "deploys")
	if len(alerts) != 1 || alerts[0].Criteria != "storage" {
		t.Fatalf("alerts = %v", alerts)
	}
	if len(deploys) != 1 || deploys[0].Criteria != "rollbacks" {
		t.Fatalf("deploys = %v", deploys)
	}
}

func TestSetReplaces(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	_ = s.Set(ctx, "alerts", "conn-1", "first")
	_ = s.Set(ctx, "alerts", "conn-1", "second")

	cs, _ := s.Candidates(ctx, "alerts")
	if len(cs) != 1 {
		t.Fatalf("got %d candidates, want 1 after replacing", len(cs))
	}
	if cs[0].Criteria != "second" {
		t.Fatalf("Criteria = %q, want the replacement", cs[0].Criteria)
	}
}

// "No interest" is expressed by not registering. An empty predicate would
// become an empty question and cost tokens to answer meaninglessly.
func TestSetRejectsEmptyInput(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	for _, tc := range []struct{ name, topic, conn, pred string }{
		{"empty predicate", "alerts", "conn-1", ""},
		{"empty topic", "", "conn-1", "storage"},
		{"empty connID", "alerts", "", "storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Set(ctx, tc.topic, tc.conn, tc.pred); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// Drop is the normal cleanup path: a disconnect must clear that
// connection's interests on every topic, without touching anyone else's.
func TestDropRemovesAcrossTopics(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	_ = s.Set(ctx, "alerts", "conn-1", "storage")
	_ = s.Set(ctx, "deploys", "conn-1", "rollbacks")
	_ = s.Set(ctx, "alerts", "conn-2", "network")

	if err := s.Drop(ctx, "conn-1"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	if got := ids(t, s, "alerts"); len(got) != 1 || got[0] != "conn-2" {
		t.Fatalf("alerts = %v, want only conn-2", got)
	}
	if got := ids(t, s, "deploys"); len(got) != 0 {
		t.Fatalf("deploys = %v, want empty", got)
	}
}

func TestDropUnknownConnIsNotAnError(t *testing.T) {
	s, _ := newStore(t, Options{})
	if err := s.Drop(context.Background(), "never-existed"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := s.Drop(context.Background(), ""); err != nil {
		t.Fatalf("Drop(\"\"): %v", err)
	}
}

// The property the whole storage shape was chosen for. An instance killed
// without running its disconnect hook leaves interests behind; the TTL
// must retire them, and the index must converge without a sweeper.
// Otherwise every later publish pays Jev tokens to judge ghosts.
func TestExpiredInterestsSelfHeal(t *testing.T) {
	s, mr := newStore(t, Options{TTL: time.Minute})
	ctx := context.Background()

	_ = s.Set(ctx, "alerts", "crashed", "storage")
	_ = s.Set(ctx, "alerts", "alive", "network")

	if got := ids(t, s, "alerts"); len(got) != 2 {
		t.Fatalf("before expiry = %v, want 2", got)
	}

	// The live connection keeps refreshing; the crashed one cannot.
	mr.FastForward(40 * time.Second)
	if err := s.Refresh(ctx, "alive"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mr.FastForward(40 * time.Second)

	got := ids(t, s, "alerts")
	if len(got) != 1 || got[0] != "alive" {
		t.Fatalf("after expiry = %v, want only the refreshed connection", got)
	}

	// And the index must have been pruned, not merely filtered on read —
	// otherwise it grows forever with entries that MGET keeps missing.
	members, err := mr.SMembers(s.indexKey("alerts"))
	if err != nil {
		t.Fatalf("SMembers: %v", err)
	}
	if len(members) != 1 || members[0] != "alive" {
		t.Fatalf("index = %v, want the stale entry pruned", members)
	}
}

func TestRefreshExtendsTTL(t *testing.T) {
	s, mr := newStore(t, Options{TTL: time.Minute})
	ctx := context.Background()
	_ = s.Set(ctx, "alerts", "conn-1", "storage")

	for i := 0; i < 5; i++ {
		mr.FastForward(30 * time.Second)
		if err := s.Refresh(ctx, "conn-1"); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}

	// 150s elapsed against a 60s TTL; only refreshing kept it alive.
	if got := ids(t, s, "alerts"); len(got) != 1 {
		t.Fatalf("got %v, want the refreshed interest to survive", got)
	}
}

func TestRefreshUnknownConnIsNotAnError(t *testing.T) {
	s, _ := newStore(t, Options{})
	if err := s.Refresh(context.Background(), "never-existed"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

// Several deployments may share one Redis.
func TestKeyPrefixIsolates(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	a, _ := New(rdb, Options{KeyPrefix: "a:"})
	b, _ := New(rdb, Options{KeyPrefix: "b:"})
	ctx := context.Background()

	_ = a.Set(ctx, "alerts", "conn-1", "from a")
	_ = b.Set(ctx, "alerts", "conn-1", "from b")

	ca, _ := a.Candidates(ctx, "alerts")
	cb, _ := b.Candidates(ctx, "alerts")
	if len(ca) != 1 || ca[0].Criteria != "from a" {
		t.Fatalf("a = %v", ca)
	}
	if len(cb) != 1 || cb[0].Criteria != "from b" {
		t.Fatalf("b = %v", cb)
	}
}

// Topics are discovered, not registered: one exists because something
// subscribed to it, and vanishes when its last interest goes.
func TestTopicsAreDiscovered(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	if got, err := s.Topics(ctx); err != nil || len(got) != 0 {
		t.Fatalf("got (%v, %v), want no topics on an empty store", got, err)
	}

	_ = s.Set(ctx, "alerts", "c1", "storage")
	_ = s.Set(ctx, "deploys", "c1", "rollbacks")
	_ = s.Set(ctx, "alerts", "c2", "network")

	got, err := s.Topics(ctx)
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	if len(got) != 2 || got[0] != "alerts" || got[1] != "deploys" {
		t.Fatalf("topics = %v, want [alerts deploys] sorted", got)
	}

	// Dropping the last interest on a topic retires the topic.
	_ = s.Drop(ctx, "c1")
	_ = s.Drop(ctx, "c2")
	if got, _ := s.Topics(ctx); len(got) != 0 {
		t.Fatalf("topics = %v, want none once every interest is gone", got)
	}
}

func TestTopicsRespectsPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	a, _ := New(rdb, Options{KeyPrefix: "a:"})
	b, _ := New(rdb, Options{KeyPrefix: "b:"})
	ctx := context.Background()

	_ = a.Set(ctx, "only-a", "c1", "x")
	_ = b.Set(ctx, "only-b", "c1", "y")

	ta, _ := a.Topics(ctx)
	tb, _ := b.Topics(ctx)
	if len(ta) != 1 || ta[0] != "only-a" {
		t.Fatalf("a topics = %v", ta)
	}
	if len(tb) != 1 || tb[0] != "only-b" {
		t.Fatalf("b topics = %v", tb)
	}
}
