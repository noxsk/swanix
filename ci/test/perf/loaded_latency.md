# Concurrent-load connection latency comparison

Python 3 standard library only; rebuild perfload from this workspace for p95 output.
The tool does not change algorithm configuration.
Targets must speak the **perfload serve** protocol, not HTTP. Default target is
`127.0.0.1:9000`; no external service is contacted unless explicitly supplied.

```sh
go build -o /tmp/perfload ./ci/test/perf/perfload
python3 -m unittest discover -s ci/test/perf -p 'test_loaded_latency.py' -v

# Start this in another terminal; stop it afterwards.
/tmp/perfload serve -listen 127.0.0.1:9000 -banner loopback
python3 ci/test/perf/loaded_latency.py --perfload /tmp/perfload \
  --rounds 2 --output /tmp/loaded-single

# Supply real controlled test paths yourself:
python3 ci/test/perf/loaded_latency.py --perfload /tmp/perfload \
  --baseline 127.0.0.1:9000 --candidate 127.0.0.1:9001 \
  --rounds 6 --output /tmp/loaded-paired
```

Each round measures baseline then candidate (AB), reversing on even rounds (BA).
A single target instead runs once per round. Both targets use identical load
settings. Bulk starts first, warms up, and continues through the entire connect
process including its final in-flight operations. Defaults: 8 bulk streams,
32 connect workers, 7s warmup, 8s connect, 8s bulk tail, 1s cooldown. Adjust with
`--streams`, `--concurrency`, `--warmup`, `--duration`, `--tail`, `--timeout`,
`--cooldown`, and optionally `--src`.

The tool preserves each process's unmodified stdout JSON and stderr, commands,
monotonic timing offsets, parsed errors, and per-run metadata. `report.json` is
updated after every run. It includes per-run bulk Gbit/s, connect CPS, p50/p95/p99,
target medians, individual candidate/baseline paired ratios and their medians.
Latency ratios below 1 are better; throughput/CPS ratios above 1 are better.
These are medians of per-run percentiles, not pooled connection percentiles.
perfload uses sorted successful connect+banner durations at index
`floor(p * (n - 1))`; these are not packet RTT measurements. Old binaries without
p95 are rejected, not interpolated. Missing, nonfinite or unordered p50/p95/p99
values also reject a run.
Bulk throughput covers warmup + connect + tail, not just the connect interval.

Nonzero process exits, malformed JSON/results, any reported load error, zero
successful traffic, a zero-throughput bulk stream, and incomplete temporal
coverage reject a run. Rejected runs are retained but excluded from statistics;
a pair is included only if BOTH runs are valid. Any rejection makes the command
exit 1, even when other pairs are valid. Output directories must be new to avoid
mixing experiments. Raw files survive failures; interrupted runs may not yet
appear in the aggregate report.

## Interpretation and limits

- Two addresses **do not configure or prove algorithm versions**. Deploy and
  record versions, routing, link capacity, source addresses and identical server
  settings independently. A loopback run validates tooling, not algorithm gains.
- This establishes concurrent offered load, not proof of bottleneck saturation.
  Increase streams until throughput plateaus and verify link utilization and
  client/server CPU externally before calling it a full-load comparison.
- perfload has no readiness/stream-liveness telemetry. Warmup defaults exceed its
  nominal dial + banner timeout budget (3s each); all streams must show positive
  throughput and no errors. Timing checks conservatively require the connect
  process to finish before bulk's launch + requested duration, with bulk still
  alive. They cannot prove every stream continuously transferred throughout the
  measurement. Increase warmup/tail for slow startup/scheduling; investigate
  rejected runs rather than selecting only favorable pairs.
- Alternating order reduces drift bias but is not a significance test. Keep all
  rounds, inspect error/rejection counts, use enough repetitions and compare
  throughput alongside latency to avoid mistaking reduced offered load for gains.
