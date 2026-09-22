---
title: semantic-pubsub-jev - Measurements
tags:
  - project
  - semantic-pubsub-jev
status: active
---

# Measurements

The project's actual deliverable. Reproduce any of these with
`cmd/measure`; raw responses are written to `results/` so a run happens
once and can be re-analysed for free.

## M1 — judgment stability

**Question:** publish the same message repeatedly against the same stated
interests. Does it route the same way every time?

**Why it decides viability:** a router that is not reproducible is not a
router. If a message reaches a subscriber on one publish and not the
next, no amount of accuracy elsewhere rescues it.

### Method

7 messages x 6 interests, judged in a single request per message, **20
repeats**. 20 message/interest pairs are scored against expectations.
140 requests, 122,500 input tokens, **$0.005145**, 1m22s.

Model pinned to `jev-1.13.0` — an alias moves on its own schedule and a
run spanning two versions would measure the release, not the stability.

Fixtures deliberately include awkward cases, because a stability run over
obviously separable interests would report perfect agreement and tell you
nothing:

- a **near miss** — a routine deploy notice naming a service that a
  subscriber watches for *outages*
- **genuine ambiguity** — "succeeding but 8% slower", against "hard
  outages" and "anything customers would notice"
- a **region mismatch** — a `us-east-1` alert against a subscriber
  watching `eu-west`
- a **cascade** matching four interests at once

### Result

**Flip rate: 0.0%. Zero of 20 cases were non-unanimous across 20
repeats.**

Standard deviation never exceeded **0.0168**, and seven cases returned
byte-identical probabilities on all 20 runs.

Every expectation was met. No case was stable but wrong.

| Case | Expected | Decision | Mean | StdDev |
| --- | --- | --- | --- | --- |
| `disk-full/storage` | deliver | 20/20 | 0.979 | 0.0036 |
| `bruteforce/security` | deliver | 20/20 | 0.970 | 0.0000 |
| `cascade/outages` | deliver | 20/20 | 0.970 | 0.0000 |
| `latency/network` | deliver | 20/20 | 0.960 | 0.0000 |
| `us-east-outage/eu` | skip | 0/20 | 0.040 | 0.0000 |
| `deploy-mentions-checkout/outages` | skip | 0/20 | 0.040 | 0.0000 |
| `bruteforce/storage` | skip | 0/20 | 0.030 | 0.0000 |
| `partial-degradation/customer` | borderline | 20/20 | 0.634 | 0.0168 |

### The near miss held

`deploy-mentions-checkout/outages` scored **0.040** — a deploy notice
naming `checkout-api` was *not* delivered to a subscriber watching for
checkout outages.

That case exists to test the question wording. The judge asks whether a
message should be **delivered** to someone with a stated interest, not
whether it is **about** that subject. Topical similarity would have
matched on the service name. It did not.

### Threshold robustness

Re-analysed at 0.2, 0.3, 0.7 and 0.9 — free, since thresholds are
arithmetic over recorded probabilities:

| Threshold | Flip rate |
| --- | --- |
| 0.2 | 0.0% |
| 0.3 | 0.0% |
| 0.5 | 0.0% |
| 0.7 | 0.0% |
| 0.9 | 0.0% |

**No case straddles any threshold.** Variance is small enough that all 20
repeats land on the same side wherever the line is drawn.

This is about *stability*, not *selection*: the threshold still decides
**which** messages are delivered — a case at 0.634 delivers at 0.5 and
not at 0.7 — it just never causes the decision to become unstable.

### Caveats

**The fixtures never produced a knife-edge case.** With a maximum
standard deviation of 0.0168, a case would need its mean within roughly
0.05 of the threshold to risk flipping. None sat that close. Jev's
answers are polarised enough that genuinely marginal cases are rare,
which is itself a useful finding, but it means M1 was not stress-tested
at the boundary.

**20 repeats.** Enough to expose ordinary variance; not enough to catch a
rare event. A 1-in-500 flip would likely be invisible here.

**One model version, one afternoon.** Nothing here speaks to stability
across model releases, which is exactly why a measurement run pins a
version.

### Verdict

**M1 passes.** Routing is reproducible, and the viability gate is
cleared. Later measurements can assume that a difference in outcome
reflects a difference in input rather than noise.

## M2 — batch degradation

**Question:** latency is already known to be near-flat in question count,
which is what makes batching affordable. Is *accuracy* flat too? If a
request carrying 200 predicates answers the same questions differently
from one carrying 6, the economic argument for batching costs
correctness, and there is a practical ceiling on subscribers per topic.

### Method

Anchor cases are held fixed — the same 6 interests, the same 7 messages,
the same 20 scored pairs as M1 — while the request is padded with filler
subscribers to reach the target size. Because the anchor questions are
identical at every size, any change in their answers is attributable to
the padding and nothing else.

Filler predicates are plausible and distinct ("certificate expiry and TLS
problems", "queue backlogs and consumer lag") rather than nonsense.
Padding with gibberish would measure how the model handles gibberish
instead of how it handles scale.

Sizes 6, 25, 50, 100, 200. 5 repeats each. 175 requests, 1,315,475 input
tokens, **$0.0553**, 1m43s. Baseline is size 6.

### Result

**Zero decision changes at any size.** No anchor case routed differently
at 200 predicates than it did at 6.

| Size | Mean latency | Mean \|drift\| | Max \|drift\| | Decision changes |
| --- | --- | --- | --- | --- |
| 6 | 202ms | — | — | baseline |
| 25 | 191ms | 0.0017 | 0.0120 | 0 |
| 50 | 230ms | 0.0016 | 0.0080 | 0 |
| 100 | 221ms | 0.0018 | 0.0100 | 0 |
| 200 | 327ms | 0.0030 | 0.0140 | 0 |

**Drift stays inside the noise floor.** M1 measured per-case standard
deviation up to 0.0168 at a fixed batch size; the largest drift observed
here across a 33x change in batch size is 0.0140. The effect of adding
194 subscribers to a request is smaller than the run-to-run variance of
asking the same question twice.

Largest individual movements, all harmless:

```
cascade/storage                n=200  0.830 -> 0.816  (-0.014)
partial-degradation/customer   n=25   0.632 -> 0.620  (-0.012)
disk-full/customer             n=200  0.830 -> 0.842  (+0.012)
latency/customer               n=200  0.788 -> 0.798  (+0.010)
```

### Cost and latency at scale

| Size | Tokens/request | Mean latency | $/1,000 publishes |
| --- | --- | --- | --- |
| 6 | 875 | 202ms | $0.037 |
| 25 | 2,599 | 191ms | $0.109 |
| 50 | 4,985 | 230ms | $0.209 |
| 100 | 9,771 | 221ms | $0.410 |
| 200 | 19,355 | 327ms | $0.813 |

Cost is linear in subscriber count, as expected — tokens scale with
questions. **Latency is not.** Going from 6 to 200 subscribers, a 33x
increase in work, costs 62% more wall time. That is the property the
whole design rests on, now measured at the top end rather than
extrapolated.

### Caveats

**The drift trend is monotonic.** Mean drift rises 0.0017 → 0.0030 across
the range. It is tiny, but it is not noise-shaped, so it may well
continue past 200. This run did not find the ceiling.

**200 was not a limit, just a stopping point.** At roughly 67 tokens per
question, 200 questions is about 13k tokens against a 32k budget for
state plus the longest question. There is headroom to test further.

**5 repeats per size.** Enough to separate drift from variance given how
small both are; not enough to catch a rare outlier.

### Verdict

**M2 passes.** Batching does not degrade the answers over the range
tested. A topic can carry at least 200 semantically-routed subscribers
with no measurable loss of routing quality, one request, and roughly a
third of a second.

## M3 — threshold vs. wording

Not yet run. The threshold half is partly answered by M1: within this
fixture set, threshold choice does not affect stability. The wording half
— whether "storage problems" behaves like "issues with disks" — is
untouched.
