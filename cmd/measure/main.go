// Command measure runs the experiments this project exists to produce.
//
// M1 — judgment stability. The same messages and interests are judged
// repeatedly, and the run reports how often the routing decision changes.
// It is the viability gate: a router that is not reproducible is not a
// router.
//
//	go run ./cmd/measure -repeats 20 -dry-run     # projected cost, spends nothing
//	go run ./cmd/measure -repeats 20              # the real thing
//
// Every raw response is written to results/ as JSON lines, so a run
// happens once and can be re-analysed forever. Re-running to try a
// different threshold is waste: thresholds are applied to recorded
// probabilities by -analyse.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/joho/godotenv"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/budget"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/measure"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		repeats   = flag.Int("repeats", 20, "how many times to judge each message")
		threshold = flag.Float64("threshold", 0.5, "routing threshold whose flips are counted")
		model     = flag.String("model", "jev-1.13.0", "model to pin; an alias moves and would confound the run")
		maxSpend  = flag.Float64("budget", 0.10, "hard spend ceiling in USD")
		maxRate   = flag.Int("max-rate", 120, "hard ceiling on calls per minute")
		outDir    = flag.String("out", "results", "directory for raw responses")
		dryRun    = flag.Bool("dry-run", false, "print projected cost and exit without spending")
		analyse   = flag.String("analyse", "", "re-analyse a recorded run instead of calling the API")
	)
	flag.Parse()

	if *analyse != "" {
		return analyseRecorded(*analyse, *threshold)
	}

	requests, tokens, usd := measure.EstimateCost(*repeats, len(measure.Messages), len(measure.Interests))
	fmt.Printf("M1 — judgment stability\n\n")
	fmt.Printf("  messages    %d\n", len(measure.Messages))
	fmt.Printf("  interests   %d\n", len(measure.Interests))
	fmt.Printf("  cases       %d\n", len(measure.Cases))
	fmt.Printf("  repeats     %d\n", *repeats)
	fmt.Printf("  requests    %d\n", requests)
	fmt.Printf("  est. tokens %d\n", tokens)
	fmt.Printf("  est. cost   $%.4f   (ceiling $%.4f)\n\n", usd, *maxSpend)

	if *dryRun {
		fmt.Println("dry run: nothing was sent, nothing was spent.")
		return nil
	}
	if usd > *maxSpend {
		return fmt.Errorf("projected cost $%.4f exceeds the -budget ceiling $%.4f; raise it deliberately or lower -repeats",
			usd, *maxSpend)
	}

	_ = godotenv.Load()
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return fmt.Errorf("TYPESAFE_API_KEY is not set; put it in .env")
	}

	client, err := jev.New(jev.Options{APIKey: key, Model: *model})
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	limited, err := budget.New(client, budget.Options{
		MaxSpendUSD: *maxSpend, MaxCallsPerMin: *maxRate, Log: log,
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *outDir, err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	rawPath := filepath.Join(*outDir, fmt.Sprintf("m1-%s.jsonl", stamp))
	raw, err := os.Create(rawPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", rawPath, err)
	}
	defer raw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Pace under the rate ceiling with a margin, so the guard stays armed
	// for the case it exists for — a bug in this harness — rather than
	// being turned off to let a deliberate run through.
	interval := time.Duration(float64(time.Minute) / (float64(*maxRate) * 0.85))
	eta := time.Duration(requests) * interval

	fmt.Printf("running (raw responses -> %s)\n", rawPath)
	fmt.Printf("pacing %s between requests to stay under %d/min; ~%s\n\n",
		interval.Round(time.Millisecond), *maxRate, eta.Round(time.Second))

	report, err := measure.RunStability(ctx, limited, measure.StabilityConfig{
		Repeats: *repeats, Threshold: *threshold, Raw: raw, MinInterval: interval,
	})
	if err != nil {
		return fmt.Errorf("%w\n(partial raw output is in %s)", err, rawPath)
	}

	printReport(report)
	fmt.Printf("\nraw responses: %s\n", rawPath)
	fmt.Printf("re-analyse at another threshold without spending:\n")
	fmt.Printf("  go run ./cmd/measure -analyse %s -threshold 0.7\n", rawPath)
	return nil
}

func printReport(r *measure.StabilityReport) {
	fmt.Printf("=== M1 results ===\n\n")
	fmt.Printf("model       %s\n", r.Model)
	fmt.Printf("repeats     %d\n", r.Repeats)
	fmt.Printf("threshold   %.2f\n", r.Threshold)
	fmt.Printf("requests    %d in %s\n", r.Requests, r.Duration.Round(time.Second))
	fmt.Printf("tokens      %d  ($%.6f)\n\n", r.InputTokens, r.CostUSD)

	fmt.Printf("%-26s %-10s %-11s %6s %6s %6s %7s %s\n",
		"case", "expect", "decision", "mean", "min", "max", "stddev", "")
	for _, c := range r.Cases {
		decision := fmt.Sprintf("%d/%d", c.Delivered, len(c.Probs))
		flag := ""
		if c.Flipped {
			flag = "  <-- FLIPPED"
		}
		fmt.Printf("%-26s %-10s %-11s %6.3f %6.3f %6.3f %7.4f%s\n",
			c.MessageID+"/"+c.InterestID, c.Expect, decision,
			c.Mean, c.Min, c.Max, c.StdDev, flag)
	}

	fmt.Printf("\nflip rate   %.1f%% (%d of %d cases were not unanimous)\n",
		r.FlipRate()*100, countFlipped(r.Cases), len(r.Cases))

	if len(r.Disagreements) > 0 {
		fmt.Printf("\nstable but contrary to expectation (%d):\n", len(r.Disagreements))
		for _, c := range r.Disagreements {
			fmt.Printf("  %-26s expected %-8s got %d/%d at mean %.3f\n",
				c.MessageID+"/"+c.InterestID, c.Expect, c.Delivered, len(c.Probs), c.Mean)
		}
		fmt.Println("  (a confidently wrong answer is a different problem from an unstable one)")
	}
}

func countFlipped(cs []measure.CaseResult) int {
	n := 0
	for _, c := range cs {
		if c.Flipped {
			n++
		}
	}
	return n
}

// analyseRecorded replays a recorded run at a different threshold.
// Thresholds are arithmetic over stored probabilities, so this costs
// nothing — which is the whole reason raw responses are written.
func analyseRecorded(path string, threshold float64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	type rec struct {
		Repeat    int                `json:"repeat"`
		MessageID string             `json:"message"`
		Model     string             `json:"model"`
		Probs     map[string]float64 `json:"probs"`
	}
	obs := map[string]map[string][]float64{}
	model := ""
	repeats := map[int]bool{}

	dec := json.NewDecoder(f)
	for {
		var r rec
		if err := dec.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		model = r.Model
		repeats[r.Repeat] = true
		if obs[r.MessageID] == nil {
			obs[r.MessageID] = map[string][]float64{}
		}
		for id, p := range r.Probs {
			obs[r.MessageID][id] = append(obs[r.MessageID][id], p)
		}
	}

	fmt.Printf("=== re-analysis of %s at threshold %.2f ===\n\n", filepath.Base(path), threshold)
	fmt.Printf("model %s, %d repeats, no API calls made\n\n", model, len(repeats))

	var flipped, total int
	type row struct {
		name         string
		delivered, n int
		mean         float64
		flip         bool
	}
	var rows []row
	for _, c := range measure.Cases {
		probs := obs[c.MessageID][c.InterestID]
		if len(probs) == 0 {
			continue
		}
		var sum float64
		delivered := 0
		for _, p := range probs {
			sum += p
			if p >= threshold {
				delivered++
			}
		}
		fl := delivered != 0 && delivered != len(probs)
		rows = append(rows, row{c.MessageID + "/" + c.InterestID, delivered, len(probs), sum / float64(len(probs)), fl})
		total++
		if fl {
			flipped++
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].flip && !rows[j].flip })
	for _, r := range rows {
		flag := ""
		if r.flip {
			flag = "  <-- FLIPPED"
		}
		fmt.Printf("%-26s %d/%d  mean %.3f%s\n", r.name, r.delivered, r.n, r.mean, flag)
	}
	fmt.Printf("\nflip rate %.1f%% (%d of %d)\n", float64(flipped)/float64(total)*100, flipped, total)
	return nil
}
