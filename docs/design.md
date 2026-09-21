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

## Open questions

- **Storage shape in Redis.** A hash per topic is one round trip to read
  all candidates but grows unbounded per topic; a key per interest is
  cleaner to expire but needs a scan or an index set. Leaning toward a
  hash per topic plus a TTL-refreshing index, undecided.
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
- **Live Jev** only in the measurement harness (M1–M3), which is
  explicitly opt-in and reports token spend.

The measurement harness is a first-class deliverable, not test
scaffolding: M1–M3 are the project's actual output.
