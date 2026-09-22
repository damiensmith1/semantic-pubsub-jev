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

// M3 asks how much the *phrasing* of an interest matters.
//
// M1 settled the threshold half: no case straddled any threshold from 0.2
// to 0.9, so where the line sits never destabilises a decision. What is
// left is the harder question. If two people describing the same interest
// in different words get different messages, the system is hard to use in
// practice however stable it is — the operator's real problem becomes
// guessing the phrasing the model likes.
//
// The measurement compares two variances that are already known to be
// separable: the spread across paraphrases of one concept, against the
// run-to-run noise floor M1 measured for a single fixed phrasing. Wording
// only "matters" if it moves answers more than simply asking twice does.

// Concept is one intent expressed several ways.
type Concept struct {
	ID string

	// Phrasings range from the original to terse and verbose variants.
	// The terse ones are deliberate: a one-word interest is what a real
	// user types, and is where divergence is most likely.
	Phrasings []Phrasing
}

// Phrasing is one way of saying a concept.
type Phrasing struct {
	ID   string
	Text string
	// Style labels the variation so a result can say *what kind* of
	// rewording caused trouble, not merely that some did.
	Style string
}

// Concepts are the intents tested for wording sensitivity.
var Concepts = []Concept{
	{
		ID: "storage",
		Phrasings: []Phrasing{
			{"storage-orig", "database and storage problems, including disk capacity", "original"},
			{"storage-terse", "disk problems", "terse"},
			{"storage-plain", "issues with disks or databases running out of room", "plain language"},
			{"storage-verbose", "anything where persistent storage is failing or filling up: volumes, disks, database capacity, write failures caused by lack of space", "verbose"},
			{"storage-examples", "storage trouble, for example a disk near capacity or a database that cannot write", "with examples"},
		},
	},
	{
		ID: "outages",
		Phrasings: []Phrasing{
			{"outage-orig", "hard outages where a service is completely unavailable", "original"},
			{"outage-terse", "outages", "terse"},
			{"outage-plain", "something is completely down", "plain language"},
			{"outage-exclusive", "a service is entirely unavailable to users, not merely slow or degraded", "with exclusion"},
			{"outage-verbose", "complete loss of service availability, where requests fail outright rather than being served slowly or partially", "verbose"},
		},
	},
	{
		ID: "customer",
		Phrasings: []Phrasing{
			{"cust-orig", "anything that customers would notice or complain about", "original"},
			{"cust-terse", "customer impact", "terse"},
			{"cust-plain", "problems real users would actually see", "plain language"},
			{"cust-exclusive", "issues visible to end users, excluding purely internal or infrastructure concerns", "with exclusion"},
			{"cust-verbose", "anything a customer using the product would experience as broken, slow, or otherwise wrong, whether or not the cause is known", "verbose"},
		},
	},
}

// WordingConfig configures an M3 run.
type WordingConfig struct {
	Repeats     int
	Threshold   float64
	Raw         io.Writer
	MinInterval time.Duration
}

// PhrasingResult is one phrasing against one message.
type PhrasingResult struct {
	MessageID string
	ConceptID string
	Phrasing  Phrasing

	Mean, StdDev float64
	Delivered    int
	Repeats      int
	Deliver      bool
}

// ConceptResult is one concept against one message, across phrasings.
type ConceptResult struct {
	MessageID string
	ConceptID string
	Results   []PhrasingResult

	// Spread is max mean minus min mean across phrasings.
	Spread float64

	// NoiseFloor is the largest within-phrasing standard deviation here,
	// which is the like-for-like comparison: variance from asking the
	// same thing twice.
	NoiseFloor float64

	// Agreed reports that every phrasing routed the same way.
	Agreed bool

	// DeliverCount is how many phrasings chose to deliver.
	DeliverCount int
}

// WordingReport is a whole M3 run.
type WordingReport struct {
	Model     string
	Threshold float64
	Repeats   int
	Concepts  []ConceptResult

	Requests    int
	InputTokens int
	CostUSD     float64
	Duration    time.Duration
}

// DisagreementRate is the fraction of message/concept pairs where
// phrasings did not all route the same way.
func (r WordingReport) DisagreementRate() float64 {
	if len(r.Concepts) == 0 {
		return 0
	}
	var n int
	for _, c := range r.Concepts {
		if !c.Agreed {
			n++
		}
	}
	return float64(n) / float64(len(r.Concepts))
}

// RunWording executes M3.
//
// Every phrasing of every concept is asked in the same request, so they
// see identical state and differ only in wording.
func RunWording(ctx context.Context, asker Asker, cfg WordingConfig) (*WordingReport, error) {
	if cfg.Repeats <= 0 {
		cfg.Repeats = 8
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = judge.DefaultThreshold
	}

	questions := map[string]jev.Noul{}
	for _, c := range Concepts {
		for _, p := range c.Phrasings {
			questions[p.ID] = judge.QuestionFor(p.Text)
		}
	}

	// obs[messageID][phrasingID] = probabilities
	obs := map[string]map[string][]float64{}
	report := &WordingReport{Threshold: cfg.Threshold, Repeats: cfg.Repeats}
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
				line, _ := json.Marshal(map[string]any{
					"experiment": "m3", "repeat": r, "message": msg.ID,
					"model":        resp.Model,
					"latency_ms":   float64(resp.Latency.Microseconds()) / 1000,
					"input_tokens": resp.Usage.InputTokens,
					"at":           time.Now().UTC(),
					"probs":        resp.Probabilities,
				})
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

	for _, msg := range Messages {
		for _, c := range Concepts {
			cr := ConceptResult{MessageID: msg.ID, ConceptID: c.ID, Agreed: true}
			minMean, maxMean := math.Inf(1), math.Inf(-1)
			var firstDeliver bool
			var seen bool

			for _, p := range c.Phrasings {
				probs := obs[msg.ID][p.ID]
				if len(probs) == 0 {
					continue
				}
				m := mean(probs)
				sd := stddev(probs, m)
				pr := PhrasingResult{
					MessageID: msg.ID, ConceptID: c.ID, Phrasing: p,
					Mean: m, StdDev: sd, Repeats: len(probs),
					Deliver: m >= cfg.Threshold,
				}
				for _, v := range probs {
					if v >= cfg.Threshold {
						pr.Delivered++
					}
				}
				cr.Results = append(cr.Results, pr)

				if m < minMean {
					minMean = m
				}
				if m > maxMean {
					maxMean = m
				}
				if sd > cr.NoiseFloor {
					cr.NoiseFloor = sd
				}
				if pr.Deliver {
					cr.DeliverCount++
				}
				if !seen {
					firstDeliver, seen = pr.Deliver, true
				} else if pr.Deliver != firstDeliver {
					cr.Agreed = false
				}
			}
			if len(cr.Results) == 0 {
				continue
			}
			cr.Spread = maxMean - minMean
			report.Concepts = append(report.Concepts, cr)
		}
	}

	sort.Slice(report.Concepts, func(i, j int) bool {
		if report.Concepts[i].Agreed != report.Concepts[j].Agreed {
			return !report.Concepts[i].Agreed // disagreements first: the finding
		}
		return report.Concepts[i].Spread > report.Concepts[j].Spread
	})
	return report, nil
}

// EstimateWordingCost projects an M3 run.
func EstimateWordingCost(repeats, messages int) (requests, tokens int, usd float64) {
	const baseTokens, tokensPerQuery = 335, 67
	var questions int
	for _, c := range Concepts {
		questions += len(c.Phrasings)
	}
	requests = repeats * messages
	tokens = requests * (baseTokens + tokensPerQuery*questions)
	return requests, tokens, jev.Usage{InputTokens: tokens}.Cost()
}
