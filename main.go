// Command semantic-pubsub-jev runs a pub/sub broker that routes messages
// by what they mean rather than only by topic.
//
// Subscribers state an interest in natural language; every publish is
// judged once against all of a topic's stated interests in a single Jev
// request, and delivered only to those that matched.
//
// Configuration comes from the environment; see .env.example. With no
// TYPESAFE_API_KEY it runs on the keyword judge, which is deterministic
// and free and has none of the semantic behaviour this project exists to
// test.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Best effort: the environment may already carry everything.
	_ = godotenv.Load()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(getenv("LOG_LEVEL", "info")),
	}))
	slog.SetDefault(log)

	threshold, err := strconv.ParseFloat(getenv("MATCH_THRESHOLD", "0.5"), 64)
	if err != nil {
		return fmt.Errorf("MATCH_THRESHOLD: %w", err)
	}
	// Deliberately low. The whole planned measurement programme costs
	// about six cents, so a quarter of a dollar is ample headroom and
	// still catches a runaway before it matters. Raise it consciously.
	maxSpend, err := strconv.ParseFloat(getenv("MAX_SPEND_USD", "0.25"), 64)
	if err != nil {
		return fmt.Errorf("MAX_SPEND_USD: %w", err)
	}
	maxCallsPerMin, err := strconv.Atoi(getenv("MAX_CALLS_PER_MIN", "60"))
	if err != nil {
		return fmt.Errorf("MAX_CALLS_PER_MIN: %w", err)
	}
	judgeTimeout, err := durationMS("JUDGE_TIMEOUT_MS", 5*time.Second)
	if err != nil {
		return err
	}
	interestTTL, err := durationMS("INTEREST_TTL_MS", time.Hour)
	if err != nil {
		return err
	}

	a, err := app.New(app.Config{
		ListenAddr:     getenv("LISTEN_ADDR", ":8080"),
		MetricsAddr:    os.Getenv("METRICS_ADDR"),
		RedisAddrs:     splitCSV(getenv("REDIS_ADDRS", "localhost:6379")),
		APIKey:         os.Getenv("TYPESAFE_API_KEY"),
		Model:          os.Getenv("JEV_MODEL"),
		Threshold:      threshold,
		MaxSpendUSD:    maxSpend,
		MaxCallsPerMin: maxCallsPerMin,
		InterestTTL:    interestTTL,
		JudgeTimeout:   judgeTimeout,
		Log:            log,
	})
	if err != nil {
		return err
	}

	log.Info("listening", "addr", a.Addr())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return a.Run(ctx)
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durationMS(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of milliseconds, got %q", key, v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
