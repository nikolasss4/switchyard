# ADR 0001: Write the gateway in Go

- Status: accepted
- Date: 2026-10-31

## Context

Switchyard sits on the hot path of every model call its clients make. Whatever it
adds to latency is paid on every request, and it has to hold many long-lived
SSE streams open at once while it meters tokens, checks budgets and talks to
Redis and Postgres. The rest of this portfolio (glassbox, tracejudge, smolcall)
is Python, so Python with FastAPI was the obvious alternative.

## Decision

Go, standard library `net/http` for the server and the upstream clients, with a
small set of dependencies: pgx for Postgres, go-redis, the Prometheus client and
the OpenTelemetry SDK.

## Why

- **Overhead and tail latency.** A compiled binary with goroutines per request and
  no GIL keeps per-request overhead in the tens of microseconds. That is the
  number the benchmarks measure, so the language choice shows up directly in the
  results.
- **Streams are cheap.** A held SSE stream costs a goroutine (a few KB), not a
  thread or an event-loop slot. Context cancellation is built in, so a client
  disconnect closes the upstream connection without extra plumbing.
- **Operations.** One static binary, a distroless image of a few MB, `-race` in CI.
- **Contrast with the siblings.** The gateway is the "backend" piece of the
  portfolio. Doing it in the same language as everything else would hide that.

## Costs

- The official OpenAI and Anthropic SDK conformance tests are written in Python
  and run as a separate suite against the built binary.
- Format translation is more verbose in Go than with Python dicts. The wire types
  are written out as structs, which also makes the translation easy to test.
