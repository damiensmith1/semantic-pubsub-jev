package measure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/judge"
)

// M2 asks whether packing more subscribers into one request makes the
// answers worse.
//
// Latency is already known to be near-flat in question count, which is
// what makes batching affordable. Accuracy is not. If a request carrying
// 200 predicates answers the same questions differently from one carrying
// 6, then the economic argument for batching costs correctness, and the
// system has a practical ceiling on subscribers per topic.
//
// The design holds a few anchor cases fixed and pads the request with
// filler subscribers. The anchors are the same questions every time, so
// any change in their answers is attributable to the padding and nothing
// else.

// BatchConfig configures an M2 run.
type BatchConfig struct {
	// Sizes are the total question counts to test. Each must be at least
	// len(Interests), since the anchors are always present.
	Sizes []int

	// Repeats per size, so drift can be told from ordinary variance.
	Repeats int

	Threshold float64

	Raw io.Writer

	// MinInterval paces requests under the rate ceiling.
	MinInterval time.Duration
}

// BatchPoint is one anchor case measured at one batch size.
type BatchPoint struct {
	Size       int
	MessageID  string
	InterestID string
	Expect     Expectation

	Mean, StdDev float64
	Delivered    int
	Repeats      int

	// Drift is the change in mean from the smallest tested size, which
	// serves as the baseline.
	Drift float64

	// DecisionChanged reports that this case routes differently here than
	// it did at the baseline size. This is the finding that matters: a
	// probability that drifts without crossing the threshold is
	// interesting, one that crosses it is a broken router.
	DecisionChanged bool
}

// SizeSummary aggregates one batch size.
type SizeSummary struct {
	Size            int
	Requests        int
	InputTokens     int
	CostUSD         float64
	MeanLatency     time.Duration
	MaxAbsDrift     float64
	MeanAbsDrift    float64
	DecisionChanges int
}

// BatchReport is a whole M2 run.
type BatchReport struct {
	Model     string
	Threshold float64
	Baseline  int
	Points    []BatchPoint
	Sizes     []SizeSummary

	Requests    int
	InputTokens int
	CostUSD     float64
	Duration    time.Duration
}

// fillerPredicates pad a request to a target size.
//
// They are plausible and distinct rather than nonsense: a realistic other
// subscriber is what a real deployment would contain, and padding with
// gibberish would test how the model handles gibberish instead of how it
// handles scale.
var fillerPredicates = []string{
	"scheduled maintenance windows and planned downtime",
	"changes to billing or pricing configuration",
	"anything involving the payments provider",
	"certificate expiry and TLS problems",
	"queue backlogs and consumer lag",
	"memory pressure and out-of-memory events",
	"CI pipeline failures on the main branch",
	"changes to feature flag state",
	"third-party API rate limiting",
	"cache eviction rates and hit ratios",
	"DNS resolution failures",
	"anything affecting the mobile clients specifically",
	"cost anomalies and unexpected cloud spend",
	"data pipeline lag and stale reporting tables",
	"permission and access-control changes",
	"anything mentioning a specific customer account",
	"backup jobs that did not complete",
	"replication lag between regions",
	"container restarts and crash loops",
	"changes to rate limit configuration",
}

// buildQuestions returns the anchor questions plus filler to reach size.
func buildQuestions(size int) (map[string]jev.Noul, error) {
	if size < len(Interests) {
		return nil, fmt.Errorf("measure: size %d is below the %d anchor interests", size, len(Interests))
	}
	qs := make(map[string]jev.Noul, size)
	for _, in := range Interests {
		qs[in.ID] = judge.QuestionFor(in.Predicate)
	}
	for i := 0; len(qs) < size; i++ {
		// Cycle the filler list, suffixing so each is a distinct
		// subscriber rather than a duplicate question.
		p := fillerPredicates[i%len(fillerPredicates)]
		if i >= len(fillerPredicates) {
			p = fmt.Sprintf("%s (team %d)", p, i/len(fillerPredicates)+1)
		}
		qs[fmt.Sprintf("filler-%03d", i)] = judge.QuestionFor(p)
	}
	return qs, nil
}

// RunBatch executes M2.
func RunBatch(ctx context.Context, asker Asker, cfg BatchConfig) (*BatchReport, error) {
	if cfg.Repeats <= 0 {
		cfg.Repeats = 5
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = judge.DefaultThreshold
	}
	if len(cfg.Sizes) == 0 {
		cfg.Sizes = []int{len(Interests), 25, 50, 100, 200}
	}

	report := &BatchReport{Threshold: cfg.Threshold, Baseline: cfg.Sizes[0]}
	// probs[size][messageID][interestID] = observations
	probs := map[int]map[string]map[string][]float64{}
	latency := map[int][]time.Duration{}
	tokensBySize := map[int]int{}
	costBySize := map[int]float64{}
	start := time.Now()
	var last time.Time

	for _, size := range cfg.Sizes {
		questions, err := buildQuestions(size)
		if err != nil {
			return nil, err
		}
		probs[size] = map[string]map[string][]float64{}

		for r := 0; r < cfg.Repeats; r++ {
			for _, msg := range Messages {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if cfg.MinInterval > 0 && !last.IsZero() {
					if wait := cfg.MinInterval - time.Since(last); wait > 0 {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(wait):
						}
					}
				}
				last = time.Now()

				resp, err := asker.Ask(ctx, jev.Request{
					State:     judge.StateFor(ExperimentTopic, msg.Body),
					Questions: questions,
				})
				if err != nil {
					return nil, fmt.Errorf("measure: size %d, repeat %d, message %q: %w", size, r, msg.ID, err)
				}

				report.Requests++
				report.InputTokens += resp.Usage.InputTokens
				report.CostUSD += resp.Usage.Cost()
				tokensBySize[size] += resp.Usage.InputTokens
				costBySize[size] += resp.Usage.Cost()
				latency[size] = append(latency[size], resp.Latency)
				if report.Model == "" {
					report.Model = resp.Model
				}

				if cfg.Raw != nil {
					line, _ := json.Marshal(map[string]any{
						"experiment": "m2", "size": size, "repeat": r,
						"message": msg.ID, "model": resp.Model,
						"latency_ms":   float64(resp.Latency.Microseconds()) / 1000,
						"input_tokens": resp.Usage.InputTokens,
						"at":           time.Now().UTC(),
						// Only the anchors are recorded: filler answers
						// are noise for this experiment and would bloat
						// the file by an order of magnitude.
						"probs": anchorsOnly(resp.Probabilities),
					})
					_, _ = cfg.Raw.Write(append(line, '\n'))
				}

				if probs[size][msg.ID] == nil {
					probs[size][msg.ID] = map[string][]float64{}
				}
				for _, in := range Interests {
					if p, ok := resp.Probabilities[in.ID]; ok {
						probs[size][msg.ID][in.ID] = append(probs[size][msg.ID][in.ID], p)
					}
				}
			}
		}
	}
	report.Duration = time.Since(start)

	// Baseline means, against which drift is measured.
	baseMean := map[string]float64{}
	baseDeliver := map[string]bool{}
	for _, c := range Cases {
		obs := probs[report.Baseline][c.MessageID][c.InterestID]
		if len(obs) == 0 {
			continue
		}
		m := mean(obs)
		baseMean[c.MessageID+"/"+c.InterestID] = m
		baseDeliver[c.MessageID+"/"+c.InterestID] = m >= cfg.Threshold
	}

	for _, size := range cfg.Sizes {
		sum := SizeSummary{Size: size}
		var driftSum float64
		var driftN int

		for _, c := range Cases {
			key := c.MessageID + "/" + c.InterestID
			obs := probs[size][c.MessageID][c.InterestID]
			if len(obs) == 0 {
				continue
			}
			m := mean(obs)
			pt := BatchPoint{
				Size: size, MessageID: c.MessageID, InterestID: c.InterestID,
				Expect: c.Expect, Mean: m, StdDev: stddev(obs, m),
				Repeats: len(obs), Drift: m - baseMean[key],
			}
			for _, p := range obs {
				if p >= cfg.Threshold {
					pt.Delivered++
				}
			}
			pt.DecisionChanged = (m >= cfg.Threshold) != baseDeliver[key]
			report.Points = append(report.Points, pt)

			d := math.Abs(pt.Drift)
			driftSum += d
			driftN++
			if d > sum.MaxAbsDrift {
				sum.MaxAbsDrift = d
			}
			if pt.DecisionChanged {
				sum.DecisionChanges++
			}
		}
		if driftN > 0 {
			sum.MeanAbsDrift = driftSum / float64(driftN)
		}
		sum.Requests = cfg.Repeats * len(Messages)
		sum.InputTokens = tokensBySize[size]
		sum.CostUSD = costBySize[size]
		if ls := latency[size]; len(ls) > 0 {
			var total time.Duration
			for _, l := range ls {
				total += l
			}
			sum.MeanLatency = total / time.Duration(len(ls))
		}
		report.Sizes = append(report.Sizes, sum)
	}
	return report, nil
}

func anchorsOnly(all map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(Interests))
	for _, in := range Interests {
		if p, ok := all[in.ID]; ok {
			out[in.ID] = p
		}
	}
	return out
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64, m float64) float64 {
	var sq float64
	for _, x := range xs {
		d := x - m
		sq += d * d
	}
	return math.Sqrt(sq / float64(len(xs)))
}

// EstimateBatchCost projects an M2 run.
func EstimateBatchCost(sizes []int, repeats, messages int) (requests, tokens int, usd float64) {
	const baseTokens, tokensPerQuery = 335, 67
	for _, s := range sizes {
		r := repeats * messages
		requests += r
		tokens += r * (baseTokens + tokensPerQuery*s)
	}
	return requests, tokens, jev.Usage{InputTokens: tokens}.Cost()
}
