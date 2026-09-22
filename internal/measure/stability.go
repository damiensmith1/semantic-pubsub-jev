package measure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/jev"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/judge"
)

// ExperimentTopic is the topic name used in every experiment's state.
const ExperimentTopic = "alerts"

// Asker is the client this experiment drives.
type Asker interface {
	Ask(ctx context.Context, req jev.Request) (*jev.Response, error)
}

// StabilityConfig configures an M1 run.
type StabilityConfig struct {
	// Repeats is how many times each message is judged. Every repeat is
	// a fresh request; nothing is cached, because caching would measure
	// the cache.
	Repeats int

	// Threshold is the routing threshold whose flips are counted. The raw
	// probabilities are recorded regardless, so other thresholds can be
	// examined afterwards without paying again.
	Threshold float64

	// Raw, when set, receives one JSON line per request. Recording means
	// the run happens once and can be re-analysed forever.
	Raw io.Writer

	// MinInterval paces requests so a deliberate run does not trip the
	// rate ceiling that exists to catch loop bugs. The harness is a
	// well-behaved client by construction; disabling the guard to let it
	// run would remove the protection precisely when a bug in the harness
	// is most likely to spend money. Zero means no pacing.
	MinInterval time.Duration
}

// Observation is one probability for one case in one repeat.
type Observation struct {
	Repeat     int     `json:"repeat"`
	MessageID  string  `json:"message"`
	InterestID string  `json:"interest"`
	Prob       float64 `json:"prob"`
}

// rawRecord is what gets written to the raw log per request.
type rawRecord struct {
	Repeat      int                `json:"repeat"`
	MessageID   string             `json:"message"`
	Model       string             `json:"model"`
	LatencyMS   float64            `json:"latency_ms"`
	InputTokens int                `json:"input_tokens"`
	At          time.Time          `json:"at"`
	Probs       map[string]float64 `json:"probs"`
}

// CaseResult is the stability of one message/interest pair.
type CaseResult struct {
	MessageID  string
	InterestID string
	Expect     Expectation

	Probs []float64

	Min, Max, Mean, StdDev float64

	// Delivered counts repeats whose probability met the threshold.
	Delivered int

	// Flipped reports whether the routing decision was not unanimous.
	// This is the number that decides M1: a probability that wobbles
	// harmlessly is fine, one that wobbles across the threshold is not.
	Flipped bool

	// MarginToThreshold is how far the mean sat from the threshold.
	// Flips should concentrate near zero; flips far from it would mean
	// something worse than ordinary variance.
	MarginToThreshold float64
}

// StabilityReport is a whole M1 run.
type StabilityReport struct {
	Model       string
	Repeats     int
	Threshold   float64
	Cases       []CaseResult
	Requests    int
	InputTokens int
	CostUSD     float64
	Duration    time.Duration

	// Disagreements are cases whose unanimous decision contradicted a
	// stated expectation. Separate from flips: a confidently wrong answer
	// is a different problem from an unstable one.
	Disagreements []CaseResult
}

// FlipRate is the fraction of cases whose routing decision was not
// unanimous across repeats.
func (r StabilityReport) FlipRate() float64 {
	if len(r.Cases) == 0 {
		return 0
	}
	var n int
	for _, c := range r.Cases {
		if c.Flipped {
			n++
		}
	}
	return float64(n) / float64(len(r.Cases))
}

// RunStability executes M1.
//
// Each repeat judges every message once, with all interests as questions
// in a single request — the same shape the broker uses, so the numbers
// describe the real routing path rather than a synthetic one.
func RunStability(ctx context.Context, asker Asker, cfg StabilityConfig) (*StabilityReport, error) {
	if cfg.Repeats <= 0 {
		cfg.Repeats = 20
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = judge.DefaultThreshold
	}

	questions := make(map[string]jev.Noul, len(Interests))
	for _, in := range Interests {
		questions[in.ID] = judge.QuestionFor(in.Predicate)
	}

	// observations[messageID][interestID] = probabilities, in repeat order
	obs := map[string]map[string][]float64{}
	report := &StabilityReport{Repeats: cfg.Repeats, Threshold: cfg.Threshold}
	start := time.Now()

	var last time.Time
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
				// A fixed topic across every message: the topic is part
				// of the state the model sees, so varying it would
				// confound the thing being measured.
				State:     judge.StateFor(ExperimentTopic, msg.Body),
				Questions: questions,
			})
			if err != nil {
				return nil, fmt.Errorf("measure: repeat %d, message %q: %w", r, msg.ID, err)
			}

			report.Requests++
			report.InputTokens += resp.Usage.InputTokens
			report.CostUSD += resp.Usage.Cost()
			if report.Model == "" {
				report.Model = resp.Model
			}

			if cfg.Raw != nil {
				rec := rawRecord{
					Repeat: r, MessageID: msg.ID, Model: resp.Model,
					LatencyMS:   float64(resp.Latency.Microseconds()) / 1000,
					InputTokens: resp.Usage.InputTokens, At: time.Now().UTC(),
					Probs: resp.Probabilities,
				}
				line, _ := json.Marshal(rec)
				_, _ = cfg.Raw.Write(append(line, '\n'))
			}

			if obs[msg.ID] == nil {
				obs[msg.ID] = map[string][]float64{}
			}
			for id, p := range resp.Probabilities {
				obs[msg.ID][id] = append(obs[msg.ID][id], p)
			}
		}
	}
	report.Duration = time.Since(start)

	for _, c := range Cases {
		probs := obs[c.MessageID][c.InterestID]
		if len(probs) == 0 {
			continue
		}
		res := summarise(c, probs, cfg.Threshold)
		report.Cases = append(report.Cases, res)

		// A unanimous decision that contradicts a confident expectation
		// is a correctness problem, not a stability one.
		if !res.Flipped && c.Expect != Borderline {
			delivered := res.Delivered == len(probs)
			if (c.Expect == ShouldDeliver) != delivered {
				report.Disagreements = append(report.Disagreements, res)
			}
		}
	}

	sort.Slice(report.Cases, func(i, j int) bool {
		if report.Cases[i].Flipped != report.Cases[j].Flipped {
			return report.Cases[i].Flipped // unstable first: that is the finding
		}
		return math.Abs(report.Cases[i].MarginToThreshold) < math.Abs(report.Cases[j].MarginToThreshold)
	})
	return report, nil
}

func summarise(c Case, probs []float64, threshold float64) CaseResult {
	res := CaseResult{
		MessageID: c.MessageID, InterestID: c.InterestID,
		Expect: c.Expect, Probs: probs,
		Min: probs[0], Max: probs[0],
	}
	var sum float64
	for _, p := range probs {
		sum += p
		if p < res.Min {
			res.Min = p
		}
		if p > res.Max {
			res.Max = p
		}
		if p >= threshold {
			res.Delivered++
		}
	}
	res.Mean = sum / float64(len(probs))

	var sq float64
	for _, p := range probs {
		d := p - res.Mean
		sq += d * d
	}
	res.StdDev = math.Sqrt(sq / float64(len(probs)))

	res.Flipped = res.Delivered != 0 && res.Delivered != len(probs)
	res.MarginToThreshold = res.Mean - threshold
	return res
}

// EstimateCost projects what a run will cost before spending anything.
//
// The token model is fitted to measured usage: roughly a fixed cost for
// the state plus a per-question increment.
func EstimateCost(repeats, messages, interests int) (requests int, tokens int, usd float64) {
	const (
		baseTokens     = 335
		tokensPerQuery = 67
	)
	requests = repeats * messages
	tokens = requests * (baseTokens + tokensPerQuery*interests)
	return requests, tokens, jev.Usage{InputTokens: tokens}.Cost()
}
