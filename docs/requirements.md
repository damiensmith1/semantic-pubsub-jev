---
title: semantic-pubsub-jev - Requirements
tags:
  - project
  - semantic-pubsub-jev
status: active
---

# Requirements

## What this is

An **experiment, packaged as a library.** The deliverable is working code
plus honest measurements of whether semantic routing is good enough to
rely on. A result of "it is not" is a valid outcome and must be
reportable, not something the design is bent to avoid.

## Functional

| # | Requirement |
| --- | --- |
| F1 | A subscriber can register a natural-language **interest** for a topic it is subscribed to. |
| F2 | A subscriber may hold different interests on different topics. |
| F3 | On publish, every current interest for that topic is evaluated **in one Jev request**. |
| F4 | Messages are delivered only to subscribers whose interest matched. |
| F5 | Interests are cluster-wide state, not per-connection memory, so any instance can judge a publish. |
| F6 | A subscriber's interests are dropped when its connection closes. |
| F7 | Delivery decisions are made once at publish and carried with the message, so every instance and every replay agrees. |
| F8 | The match threshold is configurable, and the raw probability is retained rather than only the boolean. |
| F9 | A subscriber can subscribe without an interest and receive everything, as today. |

## Non-functional

| # | Requirement |
| --- | --- |
| N1 | **Cost is bounded and observable.** Tokens consumed per publish are recorded and attributable. |
| N2 | **One Jev request per publish**, never one per subscriber. |
| N3 | Judge latency is bounded; a hung call must not stall the publishing client indefinitely. |
| N4 | **Fail open by default.** If Jev is unavailable, deliver by exact-topic match rather than dropping messages. Configurable to fail closed. |
| N5 | Judge failures are counted separately from judge denials, so an outage is never mistaken for a routing decision. |
| N6 | Interest storage is bounded — dead subscribers must not accumulate and be judged forever. |
| N7 | The API key is never logged, committed, or sent anywhere but TypeSafe. |

## Measurements the experiment must produce

These are the point of the project, not a follow-up to it.

| # | Measurement | Question it answers |
| --- | --- | --- |
| M1 | **Judgment stability** — the same message and predicate, judged repeatedly | Is the routing decision reproducible, or does the same message reach different subscribers on different runs? |
| M2 | **Batch degradation** — discrimination quality as candidate count rises from 1 to the context limit | Does packing 200 predicates into one request make the answers worse? Flat *latency* is already measured; flat *accuracy* is not. |
| M3 | **Threshold vs. wording sensitivity** — varying the match threshold against varying predicate phrasing | Which matters more: where the operator draws the line, or how the subscriber phrases the sentence? If wording dominates, the system is hard to use in practice. |

M1 is the one that decides viability. A router that is not reproducible is
not a router.

## Non-goals

- **Not a general LLM gateway or agent router.** One narrow job: deciding
  delivery for pub/sub messages.
- **Not a modification to go-ws-server.** It is consumed as a tagged
  dependency, using only its public API. If something cannot be built
  without changing it, that is a finding to report, not a licence to fork.
- **Not production-ready infrastructure.** No HA story, no migration
  path, no SLA.
- **Not semantic search.** No embeddings, no vector index, no ranking.
  Standing interests judged against a stream, not a query against a
  corpus.
- **Not a replacement for exact-topic routing.** It is an addition;
  topics still do the coarse partitioning and semantics refine within one.
- **Not aiming to minimise cost.** Cost must be *measured* and bounded,
  not optimised at the expense of answering the questions above.

## Constraints

- **Go 1.25+**, matching go-ws-server.
- **Redis** for cluster-wide interest storage, already required by the
  broker.
- **Jev context limit:** 64k tokens per request, 32k for the state plus
  the single longest question. This caps how many predicates fit in one
  request and is a hard bound on M2.
- **Rate limits:** 250,000 tokens/sec and 1,200 requests/min, documented
  as subject to change without notice.
- **API key** supplied via `TYPESAFE_API_KEY` in an untracked `.env`.

## Open questions

- Is one request per publish still right when a topic's interest count
  exceeds what fits in 32k tokens? Chunking reintroduces multiple
  requests and changes the cost story.
- Should a subscriber see *why* it received a message — the probability,
  or the matched interest — or is delivery opaque?
- Does an interest need a TTL independent of connection lifetime, for
  subscribers that reconnect and expect their interest to persist?
