# semantic-pubsub-jev

Pub/sub that routes messages by **what they mean**, not just which topic
they were published to.

Subscribers state what they care about in plain language. Every published
message is judged once against all of a topic's stated interests, and
delivered only to the ones that matched.

```
alice: "database and storage problems, including disk capacity"
bob:   "network latency and connectivity issues"
carol: "security incidents and authentication failures"

published to topic "alerts":
  1. "primary volume at 96% capacity, writes will fail within the hour"
  2. "p99 round-trip time to eu-west up 400ms, packet loss 2%"
  3. "4,000 failed login attempts from a single ASN in 10 minutes"

delivered:
  alice <- 1        bob <- 2        carol <- 3
```

That run is real, against live [Jev](https://docs.typesafe.ai). It cost
$0.000074.

## Why this is possible now

Calling a language model per message per subscriber should be absurdly
expensive. Jev is a *System One* model: it returns typed answers with
calibrated probabilities instead of generated text, ingests the state
once and evaluates every question against it **in parallel**, and bills
only input tokens.

So one request carries the message plus every subscriber's predicate.
Measured: going from 1 to 100 predicates in a single request moved
latency from **193ms to 224ms** — near-flat. Cost is linear but small,
around **$0.30 per 1,000 publishes** at 100 subscribers.

## Status: an experiment, half done

The machinery works end to end. Whether semantic routing is *reliable
enough to depend on* is unmeasured, and that is the actual deliverable.

| | |
| --- | --- |
| **M1** | Judgment stability — is the routing decision reproducible? |
| **M2** | Batch degradation — do answers worsen as predicates per request grow? |
| **M3** | Threshold vs. wording — which matters more to whoever runs it? |

**M1 and M2 have now been run, and both pass.**

- **M1 — stability:** flip rate **0.0%** across 20 repeats of 20
  message/interest pairs, standard deviation never above 0.0168, and no
  case straddling any threshold from 0.2 to 0.9. Routing is reproducible.
- **M2 — batch degradation:** **zero decision changes** from 6 to 200
  predicates in one request. Drift peaked at 0.0140, smaller than the
  run-to-run variance of asking the same question twice. 200 subscribers
  judged in **327ms** for **$0.81 per 1,000 publishes**.

M3 is not yet run.

Full method, numbers and caveats: [docs/measurements.md](docs/measurements.md).

"It is not good enough" remains a valid result for what is left.

## Running it

```bash
cp .env.example .env          # add TYPESAFE_API_KEY
redis-server --port 6399 --save '' --daemonize yes

LISTEN_ADDR=:8123 REDIS_ADDRS=127.0.0.1:6399 go run .
go run ./cmd/demo 127.0.0.1:8123
```

Without an API key it falls back to a keyword judge: deterministic, free,
and with none of the semantic behaviour that is the point — useful for
development, useless as evidence.

### Protocol

Subscribe as normal, then state an interest:

```json
{ "type": "subscribe", "topic": "alerts" }
{ "type": "interest", "topic": "alerts",
  "data": { "predicate": "database and storage problems" } }
```

Interests are per connection and per topic, live in Redis so any instance
can judge a publish, and are dropped when the connection closes.

## How it fits together

The broker is [go-ws-server](https://github.com/damiensmith1/go-ws-server),
a separate project, consumed as a tagged dependency and never forked. It
is generic and knows nothing about semantics or Jev. This project supplies
four of its extension points:

| Extension point | Supplied here |
| --- | --- |
| `handler.Registry` | the `interest` verb |
| `bus.CandidateSource` | reads a topic's interests from Redis |
| `bus.Judge` | one batched Jev call per publish |
| `OnConnect` / `OnDisconnect` | TTL refresh, and cleanup |

Judging happens **once, at publish**, and the decision travels with the
message — so every instance and every replay agree. If Jev is
unavailable, routing falls back to ordinary topic delivery rather than
dropping messages.

## Docs

- [`docs/background.md`](docs/background.md) — the problem, prior art
- [`docs/requirements.md`](docs/requirements.md) — requirements, measurements, non-goals
- [`docs/design.md`](docs/design.md) — architecture, decisions and their evidence

## Tests

```bash
go test ./...                            # unit, free, no network
go test -tags=live ./internal/jev/ -v    # one real call, ~$0.000025
```

CI runs the unit tests and **compiles** the live-tagged ones without
running them — build tags are invisible to `go build` and to an untagged
`go vet`, so without an explicit tagged check a live test can quietly
stop compiling. Nothing in CI calls the real API or needs a key.

## License

[MIT](./LICENSE)
