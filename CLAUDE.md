# semantic-pubsub-jev

Semantic pub/sub: subscribers state their interest in natural language and
the broker decides, per message, who should receive it. Routing judgments
come from TypeSafe's **Jev** model.

An **experiment packaged as a library**. The deliverable is working code
plus honest measurements of whether semantic routing is reliable enough to
depend on — see the M1–M3 measurements in `docs/requirements.md`. "It is
not good enough" is a valid, reportable result.

## Context

- **Broker:** [go-ws-server](https://github.com/damiensmith1/go-ws-server)
  `v0.1.0`, a separate repo, consumed as a normal module dependency. It is
  generic and knows nothing about semantics or Jev. **Do not fork or
  vendor it.** If something cannot be built through its public API, that
  is a finding to report, not a licence to modify it.
- **Extension points used:** `bus.Judge`, `bus.CandidateSource`,
  `handler.Registry`, `OnConnect`/`OnDisconnect`.
- **Model:** `jev-latest` (`jev-1.13.0`) via `POST
  https://api.typesafe.ai/v1/systemone`. Typed answers with calibrated
  probabilities, not generated text. Input tokens billed, output free.
- **Docs:** <https://docs.typesafe.ai> — append `.md` to any page path for
  its Markdown source.

## Conventions

- Go 1.25+, matching go-ws-server.
- `TYPESAFE_API_KEY` lives in `.env`, which is gitignored. Never log it,
  commit it, or send it anywhere but TypeSafe.
- Live Jev calls cost money. Unit tests use a stubbed judge; integration
  tests use the keyword judge. Only the measurement harness calls the real
  API, opt-in, and it reports token spend.
- Commits are atomic and explain *why*, not just what. No co-authors.
- All work on `main`. No worktrees.

## Docs

`docs/` is the source of truth for intent:

- `docs/background.md` — the problem, why it is newly practical, prior art
- `docs/requirements.md` — functional, non-functional, the M1–M3
  measurements, non-goals, open questions
- `docs/design.md` — architecture, decisions already settled by
  measurement, open questions, testing approach

## Keeping docs in sync

Everything under docs/ is this project's source of truth, not a one-time
snapshot — including any file added there after initial setup, not just
background.md/requirements.md/design.md. In the SAME turn as a code
change (not a followup), update the relevant doc when you:
- resolve or add an open question in design.md
- make or change an architecture/approach decision
- add, change, or drop a requirement or non-goal
- learn something that changes the "why" in background.md
- create a new doc under docs/ for a topic that doesn't fit the above

Don't fabricate a decision that wasn't actually made. If it's unclear
whether something is doc-worthy, ask instead of guessing.
