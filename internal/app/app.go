// Package app wires the broker, the interest store and the judge into a
// running server.
//
// Everything here goes through go-ws-server's public API. The broker is a
// tagged dependency and knows nothing about semantics or Jev; this
// package supplies four of its extension points and nothing more.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/damiensmith1/go-ws-server/bus"
	"github.com/damiensmith1/go-ws-server/handler"
	"github.com/damiensmith1/go-ws-server/wsserver"
	"github.com/redis/go-redis/v9"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/interest"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/judge"
)

// VerbInterest is the protocol verb a subscriber uses to state what it
// wants. It is registered through the broker's handler registry, so the
// broker's own protocol is untouched.
const VerbInterest = "interest"

// Config configures the server.
type Config struct {
	ListenAddr  string
	RedisAddrs  []string
	MetricsAddr string

	// APIKey enables the Jev judge. Empty selects the keyword judge,
	// which is deterministic and free but has none of the semantic
	// behaviour that is the point of this project.
	APIKey string

	// Model pins a version. Empty uses the moving alias, which is fine
	// for development and wrong for a measurement run.
	Model string

	// Threshold is the probability at or above which an interest matches.
	Threshold float64

	// InterestTTL is the backstop for interests whose connection died
	// without cleanup. Refreshed at a third of this while connected.
	InterestTTL time.Duration

	// JudgeTimeout bounds one judge call. It sits on the publish path.
	JudgeTimeout time.Duration

	Log *slog.Logger
}

// App is a configured, ready-to-run server.
type App struct {
	srv       *wsserver.Server
	refresher *refresher
	jevJudge  *judge.Judge // nil when running with the keyword judge
	log       *slog.Logger
}

// New builds everything and binds the listener.
func New(cfg Config) (*App, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.InterestTTL <= 0 {
		cfg.InterestTTL = interest.DefaultTTL
	}
	if len(cfg.RedisAddrs) == 0 {
		cfg.RedisAddrs = []string{"localhost:6379"}
	}

	// The store needs its own Redis client: the broker does not expose
	// the one it uses, and sharing would couple us to its internals.
	rdb := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: cfg.RedisAddrs})
	store, err := interest.New(rdb, interest.Options{TTL: cfg.InterestTTL})
	if err != nil {
		_ = rdb.Close()
		return nil, err
	}

	routingJudge, jevJudge, err := buildJudge(cfg, log)
	if err != nil {
		_ = rdb.Close()
		return nil, err
	}

	ref := newRefresher(store, cfg.InterestTTL, log)

	reg := handler.NewRegistry()
	reg.Register(VerbInterest, interestHandler(store, log))

	srv, err := wsserver.New(wsserver.Options{
		ListenAddr:   cfg.ListenAddr,
		MetricsAddr:  cfg.MetricsAddr,
		RedisAddrs:   cfg.RedisAddrs,
		Logger:       log,
		Registry:     reg,
		Judge:        routingJudge,
		Candidates:   store,
		JudgeTimeout: cfg.JudgeTimeout,
		// Fail open: a broker that silently stops delivering when its
		// classifier is unavailable fails worse than one that briefly
		// over-delivers.
		JudgeFailurePolicy: bus.DeliverAll,

		OnConnect: func(_ context.Context, c handler.Conn) {
			ref.add(c.ConnID)
		},
		OnDisconnect: func(ctx context.Context, c handler.Conn) {
			ref.remove(c.ConnID)
			if err := store.Drop(ctx, c.ConnID); err != nil {
				log.Warn("drop interests on disconnect failed",
					"connID", c.ConnID, "err", err.Error())
			}
		},
	})
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("app: start server: %w", err)
	}

	return &App{srv: srv, refresher: ref, jevJudge: jevJudge, log: log}, nil
}

// buildJudge returns the routing judge, and the Jev judge separately when
// one was built, so cost can be reported at shutdown.
func buildJudge(cfg Config, log *slog.Logger) (bus.Judge, *judge.Judge, error) {
	if cfg.APIKey == "" {
		log.Warn("TYPESAFE_API_KEY is not set — routing with the keyword judge. " +
			"It is deterministic and free, and has none of the semantic behaviour this project exists to test.")
		return judge.Keyword{}, nil, nil
	}

	client, err := jev.New(jev.Options{APIKey: cfg.APIKey, Model: cfg.Model})
	if err != nil {
		return nil, nil, fmt.Errorf("app: jev client: %w", err)
	}
	j, err := judge.New(client, judge.Options{Threshold: cfg.Threshold, Log: log})
	if err != nil {
		return nil, nil, fmt.Errorf("app: judge: %w", err)
	}
	return j, j, nil
}

// Addr reports the resolved listen address.
func (a *App) Addr() string { return a.srv.Addr() }

// Run starts the server and the refresh loop, and blocks until ctx is
// cancelled or the server stops.
func (a *App) Run(ctx context.Context) error {
	go a.refresher.run(ctx)

	err := a.srv.Run(ctx)

	// Report what the run cost, so spend is visible without waiting for
	// an invoice.
	if a.jevJudge != nil {
		s := a.jevJudge.Stats()
		a.log.Info("judge usage",
			"calls", s.Calls, "candidates", s.Candidates,
			"inputTokens", s.InputTokens, "estimatedCostUSD", fmt.Sprintf("%.6f", s.Cost()))
	}
	return err
}

// interestPayload is the body of an interest frame.
type interestPayload struct {
	Predicate string `json:"predicate"`
}

// interestHandler records a subscriber's stated interest in a topic.
//
// Deliberately separate from subscribe rather than an extra field on it:
// the broker's subscribe is generic, and a subscriber may change its mind
// without resubscribing or replaying history. Registering an interest for
// a topic it has not subscribed to is harmless — nothing will be routed
// to it — so it is not an error worth rejecting.
func interestHandler(store *interest.Store, log *slog.Logger) handler.Handler {
	return func(ctx context.Context, req *handler.Request, _ handler.Responder) (string, error) {
		if req.Envelope == nil {
			return "", errors.New("interest requires a payload")
		}
		topic := strings.TrimSpace(req.Topic())
		if topic == "" {
			return "", errors.New("interest requires a topic")
		}

		var body interestPayload
		if len(req.Envelope.Data) > 0 {
			if err := json.Unmarshal(req.Envelope.Data, &body); err != nil {
				return "", errors.New(`interest data must be {"predicate": "..."}`)
			}
		}
		predicate := strings.TrimSpace(body.Predicate)
		if predicate == "" {
			return "", errors.New("interest requires a non-empty predicate")
		}

		if err := store.Set(ctx, topic, req.ConnID, predicate); err != nil {
			// The store's error names keys and Redis detail; the client
			// gets a stable message instead.
			log.Error("store interest failed",
				"connID", req.ConnID, "topic", topic, "err", err.Error())
			return "", errors.New("could not record interest")
		}

		log.Debug("interest registered",
			"connID", req.ConnID, "topic", topic, "predicate", predicate)
		return fmt.Sprintf("Interest registered for topic %s", topic), nil
	}
}
