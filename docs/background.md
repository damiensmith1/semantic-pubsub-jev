---
title: semantic-pubsub-jev - Background
tags:
  - project
  - semantic-pubsub-jev
status: active
---

# Background

## The problem

Conventional pub/sub routes by **exact topic match**. A subscriber picks a
topic and receives everything published to it. Narrowing that down is the
subscriber's job: it filters client-side, discarding most of what it
receives, or the publisher fragments one logical stream into many
fine-grained topics so subscribers can pick precisely.

Both are workarounds for the same missing capability. The broker has the
message in hand and knows who is listening, but has no way to reason about
whether a given message is *relevant* to a given subscriber, because
relevance is semantic and topics are strings.

## The idea

Let subscribers state their interest in **natural language**, and have the
broker decide delivery per message.

```
subscriber A: "anything indicating customer-facing checkout is degraded"
subscriber B: "only hard outages, not degradations or warnings"

published:    {"service":"checkout-api","severity":"warning",
               "message":"p99 latency 1180ms, above the 800ms SLO"}

delivered to: A
```

The routing decision moves from the subscriber's filter into the broker.

## Why this is newly practical

The obvious objection is cost and latency: calling a language model on
every message, for every subscriber, is absurd.

[TypeSafe](https://docs.typesafe.ai)'s **Jev** changes that arithmetic. It
is a *System One* model: it returns typed answers with calibrated
probabilities rather than generated text, it ingests state once and
evaluates every question against it **in parallel**, and output tokens
are free.

A spike against the live API measured this directly:

| Candidates in one request | Latency | Input tokens |
| --- | --- | --- |
| 1 | 193ms | 402 |
| 10 | 227ms | 1,005 |
| 100 | 224ms | 7,053 |

**Latency is flat in candidate count.** One request carrying 100
subscribers' predicates costs about the same wall time as one carrying a
single predicate. Cost is linear but small: roughly **$0.30 per 1,000
publishes** at 100 subscribers.

The same spike confirmed the judgments discriminate sensibly — for the
checkout-latency message above, the four test predicates scored 0.96,
0.04, 0.03 and 0.16.

That combination — flat latency, free output, calibrated probabilities —
is what makes per-message semantic routing worth attempting rather than
merely imaginable.

## Prior art, and how this differs

- **Content-based routing** (JMS selectors, RabbitMQ headers exchange,
  NATS subject wildcards) filters on structured fields with exact
  predicates. It cannot express "customer-facing degradation" over a
  payload that never uses those words.
- **Semantic search / RAG** matches a query against a corpus by embedding
  similarity, at read time. This is the inverse: a stream of new messages
  matched against a set of standing interests, at write time, with a
  yes/no decision rather than a ranking.
- **LLM-based routers** in agent frameworks typically classify one input
  into one of N handlers. Here every subscriber gets an independent
  yes/no, several can match, and the fan-out shape is what makes it
  affordable.

The closest framing is a **standing-query** system, where queries are
registered and documents stream past, except the query is a sentence
rather than a boolean expression.

## The substrate

The broker is [go-ws-server](https://github.com/damiensmith1/go-ws-server)
`v0.1.0`, a separate project. It was extended during this work with the
extension points this project needs — `bus.Judge`, `bus.CandidateSource`,
`handler.Registry` and connection lifecycle hooks — all of which are
generic and carry no knowledge of semantics or of Jev.

That separation is deliberate. go-ws-server stays a standalone, generic
WebSocket server; this project is one consumer of its extension points.
It is imported as a normal Go module dependency, not vendored or forked.

## Related concepts

Routing once at publish and carrying the decision with the message is a
consistency argument, and touches [[Eventual Consistency]]: every
instance and every replay must agree on a delivery decision that a
non-deterministic judge would otherwise answer differently each time.
Question construction draws on [[prompting]].
