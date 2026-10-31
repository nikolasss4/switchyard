# ADR 0003: Retry and fall back only before the first token

- Status: accepted
- Date: 2026-10-31

## Context

Providers fail in three ways: they refuse the request (connection error, 429,
5xx), they accept it and fail before generating (Anthropic can send
`overloaded_error` right after `message_start`), or they fail mid-generation.

## Decision

- Retry the same target on retryable errors (transport errors, timeouts, 408,
  409, 429, 5xx) with full-jitter exponential backoff, honouring `Retry-After`;
  a `Retry-After` longer than `max_delay` falls back at once.
- Fall back to the next target on retryable errors, and also on 401/403/404 (a
  wrong upstream key or a missing model is that provider's problem). Never on
  400/413/422: a malformed request fails the same way everywhere.
- For streams, an attempt counts as successful only once its first content
  event has been read. Events before it (`message_start`, `ping`) are buffered,
  so a failure there is retried invisibly.
- After the first content event is forwarded, a failure is reported in-band
  (an `error` event, or an OpenAI-style error chunk) and nothing is retried.

## Why never after the first token

The client has already shown or acted on the partial output. A retry produces a
different completion; splicing it on, or restarting silently, gives the user
text that no model produced. Tool calls make it worse: a half-streamed tool call
may already have been dispatched. Only the client can decide what to do, so it
gets the error.

## Consequences

- Time to first token through a failing provider includes the retries.
- Hedging uses the same rule: a hedged stream lane wins on its first content
  event, and the losing lane is cancelled and closed.
