# ADR 0002: Token buckets in Redis, one Lua script per request

- Status: accepted
- Date: 2026-10-31

## Context

Keys have requests-per-minute and tokens-per-minute limits, and the gateway
runs as several instances. A per-instance limiter lets a key use N times its
limit with N replicas. Token counts are only known after the response.

## Decision

- A token bucket per key and dimension (`<key>:rpm`, `<key>:tpm`) in Redis,
  refilled continuously (capacity = the per-minute limit).
- One Lua script checks every bucket of a request and takes from all of them or
  none, using `redis.call('TIME')`, so instances with skewed clocks agree and a
  request denied on TPM does not burn an RPM token.
- TPM is admitted on an estimate (about four characters per token of input) and
  settled afterwards with the provider-reported total. The bucket may go into
  debt down to minus its capacity, which delays the key's next requests rather
  than letting a large response escape the limit.
- A request whose estimate exceeds the bucket's capacity is refused outright
  instead of waiting forever.
- If Redis is unreachable the gateway allows the request and logs it (fail
  open). An outage of the limiter should not take down every tenant.

## Alternatives

- Fixed or sliding windows: simpler but bursty at window edges.
- Reserving `max_tokens` up front: exact, but refuses most long-output requests
  under modest TPM limits.
- A local limiter with periodic Redis sync: fewer round trips, but loose limits
  under bursts. One round trip (~0.1 ms on a local network) is cheap next to a
  model call.
