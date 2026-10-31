# Benchmarks

Everything here runs against the mock provider (`cmd/mockllm`), so the numbers
measure the gateway, not a model. The harness is `cmd/swbench`.

| Benchmark | What it measures | How |
|---|---|---|
| `overhead` | Latency the gateway adds over calling the mock directly | Constant request rate (open model, vegeta) at 250 to 4000 req/s for 20 s each, direct then through the gateway. The mock answers in 20 ms. Both surfaces: OpenAI to an OpenAI-compatible upstream, Anthropic to an Anthropic upstream |
| `throughput` | Max sustained requests per second, gateway CPU and memory | Closed loop: 16 to 1024 workers, each sending its next request as soon as the last returns, 15 s per level. The mock answers instantly |
| `stream` | Time to first token added; concurrent streams held | TTFT: 400 streams per surface, 20 in flight, mock 100 ms to first byte. Concurrency: 1k, 5k and 10k streams opened over 2 s, each about 10 s long |
| `chaos` | Success rate while the primary provider fails | 200 req/s for 100 s at an alias with a primary and a fallback mock. The primary gets 50% errors at 20 s, goes fully down at 45 s, recovers at 70 s. The same load is also sent straight to the primary as a baseline |

Every request goes through the full pipeline: key auth, the budget check and
the RPM/TPM buckets (the bench key has limits high enough never to refuse),
routing, the breaker, accounting, a usage record and metrics.

## Results

- [`results/gha-4vcpu/`](results/gha-4vcpu): GitHub Actions `ubuntu-latest` runner
  (4 vCPU AMD EPYC 7763, 16 GB), with Redis (host networking) and Postgres. This
  is the run quoted in the main README.
- [`results/m6-local/`](results/m6-local): Apple M6 Mac mini (12 cores, 32 GB),
  with no Redis (the in-process limiter and spend counter) and a local Postgres.

Both runs are at commit `59162a3`. Each directory has `environment.json`
(machine, Go version and commit), one JSON file per benchmark and, for the
runner, `lscpu`, `free` and the log.

Caveats:

- The load generator, the gateway, both mocks, Postgres and Redis share one
  machine. On the 4-vCPU runner they compete for CPU, so the gateway's ceiling
  there is partly the machine's.
- GitHub runners are shared VMs. Two runs a few minutes apart peaked at about
  7.6k and 5.2k req/s. Treat the runner numbers as one sample, not a constant.
- On the Mac, macOS defaults (an accept backlog of 128 and about 16k ephemeral
  ports) start refusing connections at 1,024 closed-loop workers and at 10k
  streams. Those rows measure the OS, not the gateway; the runner has the limits
  raised and serves both.
- Below about 1k req/s the Mac's "added latency" includes CPU wake-up from idle;
  above 2k req/s the difference is inside the measurement noise.

## Run it

```bash
go build -o bin/ ./cmd/...
./bin/mockllm -addr 127.0.0.1:9101 -name mock-a &
./bin/mockllm -addr 127.0.0.1:9102 -name mock-b &
DATABASE_URL=postgres://... REDIS_URL=redis://localhost:6379/0 SWITCHYARD_ADMIN_TOKEN=t \
  ./bin/switchyard -config configs/bench.yaml &
SWITCHYARD_ADMIN_TOKEN=t ./bin/swbench -out bench/out            # all four, about 12 minutes
SWITCHYARD_ADMIN_TOKEN=t ./bin/swbench -quick -out bench/out    # a 1-minute harness check
uv run --no-project --with matplotlib bench/charts.py bench/out docs/img
```

Or run the `bench` workflow from the Actions tab and download its artifact.

## Semantic cache eval

See [`semantic/README.md`](semantic/README.md).
