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

Not yet run.

## M3 — threshold vs. wording

Not yet run. The threshold half is partly answered above: within this
fixture set, threshold choice does not affect stability. The wording half
— whether "storage problems" behaves like "issues with disks" — is
untouched.
