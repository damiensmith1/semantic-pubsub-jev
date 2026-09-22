// Command measure runs the experiments this project exists to produce.
//
// M1 — judgment stability. The same messages and interests are judged
// repeatedly, and the run reports how often the routing decision changes.
// It is the viability gate: a router that is not reproducible is not a
// router.
//
// M2 — batch degradation. Anchor cases are held fixed while the request
// is padded with filler subscribers, so any change in their answers is
// attributable to the padding.
//
//	go run ./cmd/measure -m1 -repeats 20 -dry-run   # projected cost, spends nothing
//	go run ./cmd/measure -m1 -repeats 20            # the real thing
//	go run ./cmd/measure -m2 -repeats 5
//	go run ./cmd/measure -m3 -repeats 8
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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
		m1        = flag.Bool("m1", false, "run M1: judgment stability")
		m2        = flag.Bool("m2", false, "run M2: batch degradation")
		m3        = flag.Bool("m3", false, "run M3: wording sensitivity")
		sizes     = flag.String("sizes", "6,25,50,100,200", "M2 batch sizes (total questions per request)")
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
	chosen := 0
	for _, b := range []bool{*m1, *m2, *m3} {
		if b {
			chosen++
		}
	}
	if chosen == 0 {
		return fmt.Errorf("choose an experiment: -m1 (stability), -m2 (batch degradation) or -m3 (wording sensitivity)")
	}
	if chosen > 1 {
		return fmt.Errorf("run one experiment at a time")
	}

	if *m3 {
		return runM3(*repeats, *threshold, *model, *maxSpend, *maxRate, *outDir, *dryRun)
	}
	if *m2 {
		return runM2(*sizes, *repeats, *threshold, *model, *maxSpend, *maxRate, *outDir, *dryRun)
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

func parseSizes(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad size %q", p)
		}
		out = append(out, n)
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("need at least two sizes to measure drift between them")
	}
	sort.Ints(out)
	return out, nil
}

func runM2(sizesArg string, repeats int, threshold float64, model string,
	maxSpend float64, maxRate int, outDir string, dryRun bool) error {

	sizes, err := parseSizes(sizesArg)
	if err != nil {
		return err
	}
	requests, tokens, usd := measure.EstimateBatchCost(sizes, repeats, len(measure.Messages))

	fmt.Printf("M2 — batch degradation\n\n")
	fmt.Printf("  sizes       %v  (baseline %d)\n", sizes, sizes[0])
	fmt.Printf("  messages    %d\n", len(measure.Messages))
	fmt.Printf("  anchors     %d interests, %d cases\n", len(measure.Interests), len(measure.Cases))
	fmt.Printf("  repeats     %d per size\n", repeats)
	fmt.Printf("  requests    %d\n", requests)
	fmt.Printf("  est. tokens %d\n", tokens)
	fmt.Printf("  est. cost   $%.4f   (ceiling $%.4f)\n\n", usd, maxSpend)

	if dryRun {
		fmt.Println("dry run: nothing was sent, nothing was spent.")
		return nil
	}
	if usd > maxSpend {
		return fmt.Errorf("projected cost $%.4f exceeds the -budget ceiling $%.4f; raise it deliberately or lower -repeats", usd, maxSpend)
	}

	_ = godotenv.Load()
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return fmt.Errorf("TYPESAFE_API_KEY is not set; put it in .env")
	}
	client, err := jev.New(jev.Options{APIKey: key, Model: model})
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	limited, err := budget.New(client, budget.Options{
		MaxSpendUSD: maxSpend, MaxCallsPerMin: maxRate, Log: log,
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	rawPath := filepath.Join(outDir, fmt.Sprintf("m2-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	raw, err := os.Create(rawPath)
	if err != nil {
		return err
	}
	defer raw.Close()

	interval := time.Duration(float64(time.Minute) / (float64(maxRate) * 0.85))
	fmt.Printf("running (raw responses -> %s)\n", rawPath)
	fmt.Printf("pacing %s between requests; ~%s\n\n",
		interval.Round(time.Millisecond), (time.Duration(requests) * interval).Round(time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	report, err := measure.RunBatch(ctx, limited, measure.BatchConfig{
		Sizes: sizes, Repeats: repeats, Threshold: threshold,
		Raw: raw, MinInterval: interval,
	})
	if err != nil {
		return fmt.Errorf("%w\n(partial raw output is in %s)", err, rawPath)
	}

	printBatchReport(report)
	fmt.Printf("\nraw responses: %s\n", rawPath)
	return nil
}

func printBatchReport(r *measure.BatchReport) {
	fmt.Printf("=== M2 results ===\n\n")
	fmt.Printf("model      %s\n", r.Model)
	fmt.Printf("threshold  %.2f   baseline size %d\n", r.Threshold, r.Baseline)
	fmt.Printf("requests   %d in %s\n", r.Requests, r.Duration.Round(time.Second))
	fmt.Printf("tokens     %d  ($%.6f)\n\n", r.InputTokens, r.CostUSD)

	fmt.Printf("%-6s %10s %13s %12s %8s %12s %10s\n",
		"size", "latency", "mean |drift|", "max |drift|", "flips", "tokens/req", "$/1k pub")
	for _, s := range r.Sizes {
		perReq := 0
		if s.Requests > 0 {
			perReq = s.InputTokens / s.Requests
		}
		perThousand := 0.0
		if s.Requests > 0 {
			perThousand = s.CostUSD / float64(s.Requests) * 1000
		}
		fmt.Printf("%-6d %10s %13.4f %12.4f %8d %12d %10.4f\n",
			s.Size, s.MeanLatency.Round(time.Millisecond),
			s.MeanAbsDrift, s.MaxAbsDrift, s.DecisionChanges, perReq, perThousand)
	}

	// Only the cases that actually moved are worth printing in full.
	fmt.Printf("\nlargest drifts:\n")
	type mv struct {
		name  string
		size  int
		base  float64
		mean  float64
		drift float64
		flip  bool
	}
	var moved []mv
	for _, p := range r.Points {
		if p.Size == r.Baseline {
			continue
		}
		moved = append(moved, mv{p.MessageID + "/" + p.InterestID, p.Size,
			p.Mean - p.Drift, p.Mean, p.Drift, p.DecisionChanged})
	}
	sort.Slice(moved, func(i, j int) bool {
		return math.Abs(moved[i].drift) > math.Abs(moved[j].drift)
	})
	shown := 0
	for _, m := range moved {
		if shown >= 8 {
			break
		}
		flag := ""
		if m.flip {
			flag = "  <-- DECISION CHANGED"
		}
		fmt.Printf("  %-30s n=%-4d %.3f -> %.3f  (%+.3f)%s\n",
			m.name, m.size, m.base, m.mean, m.drift, flag)
		shown++
	}

	var totalFlips int
	for _, s := range r.Sizes {
		totalFlips += s.DecisionChanges
	}
	fmt.Printf("\ndecision changes vs baseline: %d\n", totalFlips)
}

func runM3(repeats int, threshold float64, model string,
	maxSpend float64, maxRate int, outDir string, dryRun bool) error {

	requests, tokens, usd := measure.EstimateWordingCost(repeats, len(measure.Messages))
	var phrasings int
	for _, c := range measure.Concepts {
		phrasings += len(c.Phrasings)
	}

	fmt.Printf("M3 — wording sensitivity\n\n")
	fmt.Printf("  concepts    %d\n", len(measure.Concepts))
	fmt.Printf("  phrasings   %d total\n", phrasings)
	fmt.Printf("  messages    %d\n", len(measure.Messages))
	fmt.Printf("  pairs       %d message/concept\n", len(measure.Messages)*len(measure.Concepts))
	fmt.Printf("  repeats     %d\n", repeats)
	fmt.Printf("  requests    %d\n", requests)
	fmt.Printf("  est. tokens %d\n", tokens)
	fmt.Printf("  est. cost   $%.4f   (ceiling $%.4f)\n\n", usd, maxSpend)

	if dryRun {
		fmt.Println("dry run: nothing was sent, nothing was spent.")
		return nil
	}
	if usd > maxSpend {
		return fmt.Errorf("projected cost $%.4f exceeds the -budget ceiling $%.4f", usd, maxSpend)
	}

	_ = godotenv.Load()
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return fmt.Errorf("TYPESAFE_API_KEY is not set; put it in .env")
	}
	client, err := jev.New(jev.Options{APIKey: key, Model: model})
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	limited, err := budget.New(client, budget.Options{
		MaxSpendUSD: maxSpend, MaxCallsPerMin: maxRate, Log: log,
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	rawPath := filepath.Join(outDir, fmt.Sprintf("m3-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	raw, err := os.Create(rawPath)
	if err != nil {
		return err
	}
	defer raw.Close()

	interval := time.Duration(float64(time.Minute) / (float64(maxRate) * 0.85))
	fmt.Printf("running (raw responses -> %s)\n", rawPath)
	fmt.Printf("pacing %s between requests; ~%s\n\n",
		interval.Round(time.Millisecond), (time.Duration(requests) * interval).Round(time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	report, err := measure.RunWording(ctx, limited, measure.WordingConfig{
		Repeats: repeats, Threshold: threshold, Raw: raw, MinInterval: interval,
	})
	if err != nil {
		return fmt.Errorf("%w\n(partial raw output is in %s)", err, rawPath)
	}

	printWordingReport(report)
	fmt.Printf("\nraw responses: %s\n", rawPath)
	return nil
}

func printWordingReport(r *measure.WordingReport) {
	fmt.Printf("=== M3 results ===\n\n")
	fmt.Printf("model      %s\n", r.Model)
	fmt.Printf("threshold  %.2f   repeats %d\n", r.Threshold, r.Repeats)
	fmt.Printf("requests   %d in %s\n", r.Requests, r.Duration.Round(time.Second))
	fmt.Printf("tokens     %d  ($%.6f)\n\n", r.InputTokens, r.CostUSD)

	var disagreed int
	var maxSpread, sumSpread, maxNoise float64
	for _, c := range r.Concepts {
		if !c.Agreed {
			disagreed++
		}
		sumSpread += c.Spread
		if c.Spread > maxSpread {
			maxSpread = c.Spread
		}
		if c.NoiseFloor > maxNoise {
			maxNoise = c.NoiseFloor
		}
	}

	fmt.Printf("%-34s %8s %10s %9s %s\n", "message / concept", "deliver", "spread", "noise", "")
	for _, c := range r.Concepts {
		flag := ""
		if !c.Agreed {
			flag = "  <-- PHRASINGS DISAGREE"
		}
		fmt.Printf("%-34s %4d/%-3d %10.3f %9.4f%s\n",
			c.MessageID+"/"+c.ConceptID,
			c.DeliverCount, len(c.Results), c.Spread, c.NoiseFloor, flag)
	}

	fmt.Printf("\ndisagreement rate  %.1f%% (%d of %d message/concept pairs)\n",
		r.DisagreementRate()*100, disagreed, len(r.Concepts))
	fmt.Printf("mean spread        %.4f\n", sumSpread/float64(max(len(r.Concepts), 1)))
	fmt.Printf("max spread         %.4f\n", maxSpread)
	fmt.Printf("max noise floor    %.4f   (run-to-run, same phrasing)\n", maxNoise)
	if maxNoise > 0 {
		fmt.Printf("\nwording moves answers %.1fx as much as repetition does.\n", maxSpread/maxNoise)
	}

	if disagreed > 0 {
		fmt.Printf("\nwhere phrasings disagreed:\n")
		for _, c := range r.Concepts {
			if c.Agreed {
				continue
			}
			fmt.Printf("\n  %s / %s\n", c.MessageID, c.ConceptID)
			rs := append([]measure.PhrasingResult(nil), c.Results...)
			sort.Slice(rs, func(i, j int) bool { return rs[i].Mean > rs[j].Mean })
			for _, p := range rs {
				mark := "skip   "
				if p.Deliver {
					mark = "DELIVER"
				}
				fmt.Printf("    %-7s %.3f  %-16s %q\n", mark, p.Mean, "["+p.Phrasing.Style+"]", p.Phrasing.Text)
			}
		}
	}
}
