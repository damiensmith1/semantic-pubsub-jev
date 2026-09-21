---
title: semantic-pubsub-jev - Design
tags:
  - project
  - semantic-pubsub-jev
status: active
---

# Design

## Shape

This project is a **Go program that embeds go-ws-server** and supplies four
of its extension points. It adds no code to the broker.

```
                    ┌─────────────────────────────────────────┐
  client ──ws──▶    │  go-ws-server v0.1.0  (generic)         │
                    │                                         │
                    │   handler.Registry ──────┐              │
                    │   bus.CandidateSource ───┤              │
                    │   bus.Judge ─────────────┤              │
                    │   OnConnect/OnDisconnect ┘              │
                    └──────────────┬──────────────────────────┘
                                   │  (this project implements them)
                    ┌──────────────▼──────────────────────────┐
                    │  interest store (Redis)                 │
                    │  jev client  ──https──▶  api.typesafe.ai│
                    └─────────────────────────────────────────┘
```

| Extension point | What this project supplies |
| --- | --- |
| `handler.Registry` | An `interest` verb carrying a subscriber's predicate |
| `bus.CandidateSource` | Reads cluster-wide interests for a topic from Redis |
| `bus.Judge` | One batched Jev call, returns per-subscriber decisions |
| `OnDisconnect` | Drops that connection's interests |

This shape was **validated end to end** against the real broker before
this repo existed: two subscribers with different interests, one publish,
correct selective delivery, with a keyword matcher standing in for Jev.

## Decisions already made, and why

These came out of measurement during the go-ws-server work. They are
settled unless new evidence contradicts them.

### Judge once, at publish

The decision is made on the publishing instance, before the message is
written to the stream, and the recipient list travels with the message.

Judging at delivery time instead would break three things. Every instance
receives every publish, so a non-deterministic judge would deliver to a
subscriber on one instance and not another. Replay reads from a Redis
stream, so a decision not stored there is lost on reconnect. Fan-out is
at-least-once, so a recomputed decision can differ between delivery
attempts for the same message.

### Batched, never per-subscriber

Jev ingests state once and evaluates all questions in parallel. Measured:
1 → 100 candidates moved latency from 193ms to 224ms. Per-subscriber
calls would have been 100 sequential round trips for the same work.

### Fail open

A broker that silently stops delivering when its classifier is
unavailable is a worse failure than one that briefly over-delivers. On
Jev error or timeout, routing falls back to exact-topic match. This is
configurable, because a deployment where over-delivery leaks information
would rather drop.

### Interest storage: key per interest, plus an index set

```
int:<topic>:<connID>  -> predicate, with a TTL
idx:<topic>           -> set of connIDs interested in that topic
conn:<connID>         -> set of topics that connection has interests in
```

`Candidates` reads `SMEMBERS idx:<topic>` then `MGET` — two round trips
per publish. A hash per topic would be one, which is tempting because
this is the publish hot path.

Rejected because Redis before 7.4 has no per-field TTL (`HEXPIRE`), so an
instance killed without running its disconnect hook would leak its
subscribers' fields permanently, and **every later publish would pay Jev
tokens to judge subscribers that no longer exist**. That is exactly what
N6 forbids.

The extra round trip is under a millisecond against a judge call measured
at 200–530ms — roughly half a percent of the publish budget. Self-healing
is worth far more than that.

Consequences:

- Deletion on disconnect is the normal path; the TTL is the backstop for
  when that path never runs.
- `conn:<connID>` exists so a disconnect can find what to delete without
  scanning the keyspace.
- Entries whose key has expired are skipped **and pruned from the index**
  as they are encountered, so the index converges without a sweeper.
- The TTL must be refreshed while a connection is demonstrably alive,
  from middleware on inbound frames. It defaults to an hour: too short
  and a quiet but healthy subscriber silently stops receiving.

### Pin the model version for a measurement run

`jev-latest` is an alias that moves when a new release ships. Convenient
for development, wrong for M1: a run whose judgments came from two
different models measures the release, not the stability.

The client defaults to the alias and accepts a versioned ID. The
measurement harness must pin one. The response reports the versioned ID
that actually answered, so every result stays attributable either way.

### Noul, one per subscriber

Each interest becomes a **Noul** question — probability that a condition
holds — rather than a Choice across subscribers. Several subscribers can
match the same message, which a Choice cannot express. The returned
probability is kept, not just the thresholded boolean, so M3 can be
answered without re-running.

## Interest storage

Keyed by `(topic, connID)`. `connID` is go-ws-server's per-socket
identifier, which is also what `bus.Identified` uses for filtering and
what appears in the broker's logs — one key ties the interest, the
routing decision and the log line together.

Per-connection rather than per-user on purpose: one user may hold several
sockets with different interests.

## Verified so far

- **Interests survive a crashed instance.** An interest that stops being
  refreshed expires and is pruned from the index, while a refreshed one
  survives — verified against a clock-advanced Redis.

- **The client discriminates on live Jev.** A disk-capacity alert scored
  0.980 for a storage interest against 0.050 / 0.040 / 0.030 for network,
  security and region interests — including correctly scoring an
  eu-west subscriber low on a `us-east-1` alert, which catches a judge
  keying on topic rather than content. 593 input tokens, $0.000025,
  526ms for four predicates in one request.

## Open questions

- **What happens above the context limit.** 32k tokens caps predicates
  per request. Chunk into several requests, cap interests per topic, or
  pre-filter cheaply first? Affects N2 and the cost story.
- **Where the threshold lives.** Global, per topic, or per subscriber.
  M3 should inform this, so it stays configurable and undecided until
  then.
- **Whether delivery is explainable.** Returning the probability to the
  subscriber is useful for debugging and leaks the judge's behaviour to
  clients. Undecided.
- **Interest persistence across reconnects.** Currently dropped on
  disconnect (N6). Subscribers that reconnect frequently would have to
  re-register each time. A short TTL keyed by userKey rather than connID
  is the alternative, at the cost of F5's clean lifecycle.

## Testing approach

- **Unit tests** with a stubbed judge, so routing logic is testable
  without tokens or network.
- **A keyword judge** implementing the same interface, for integration
  tests and local development — the validated stand-in described above.
- **Live Jev** only behind the `live` build tag, so it cannot run by
  accident:

  ```bash
  go test -tags=live ./internal/jev/ -v
  ```

  These skip when no `TYPESAFE_API_KEY` is present, and log token spend
  and cost on every run. The same tag gates the measurement harness.

The measurement harness is a first-class deliverable, not test
scaffolding: M1–M3 are the project's actual output.
