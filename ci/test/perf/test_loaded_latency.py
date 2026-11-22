import argparse
import json
from pathlib import Path
import tempfile
import unittest

import loaded_latency as tool


class LoadedLatencyTests(unittest.TestCase):
    def test_paired_median_not_ratio_of_medians(self):
        rows = []
        for i, (a, b) in enumerate(((1, 2), (10, 10), (100, 50))):
            for label, value in (("baseline", a), ("candidate", b)):
                rows.append(dict(round=i, label=label, valid=True,
                                 metrics=dict.fromkeys(tool.METRICS, value)))
        rows.append(dict(round=3, label="candidate", valid=False))
        result = tool.summarize(rows, True)
        self.assertEqual(result["paired_ratio_medians"]["latency_p99_ms"], 1)
        self.assertEqual(result["paired_ratio_medians"]["latency_p95_ms"], 1)
        self.assertEqual(len(result["paired_candidate_over_baseline"]), 3)
        self.assertEqual(result["rejected_runs"], 1)
        self.assertNotIn("paired_ratio_medians", tool.summarize(rows, False))

    def test_error_and_invalid_measurement_rejection(self):
        good = dict(mode="connect", seconds=1, errors={}, ok=10,
                    connections_per_second=10, latency_p50_ms=1, latency_p95_ms=1.5, latency_p99_ms=2)
        tool.validate(good, "connect")
        for data in (None, [], 42, "result"):
            with self.subTest(data=data), self.assertRaises(ValueError):
                tool.validate(data, "connect")
        for change in ({"errors": {"timeout": 1}}, {"errors": None}, {"ok": 0},
                       {"latency_p99_ms": float("nan")}, {"latency_p50_ms": 3},
                       {"latency_p95_ms": None}, {"latency_p95_ms": float("nan")},
                       {"latency_p95_ms": .5}, {"latency_p95_ms": 3},
                       {"connections_per_second": float("inf")}, {"mode": "bulk"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                tool.validate({**good, **change}, "connect")
        with self.assertRaises(ValueError):
            tool.validate(dict(mode="bulk", seconds=1, errors={}, gbit_per_second=1,
                               slowest_stream_gbit_per_second=0), "bulk")

    def test_coverage_boundaries(self):
        self.assertTrue(tool.covered(10, 12, 15, 6, 2, True))
        for start, end, alive in ((11, 15, True), (12, 16, True), (12, 15, False)):
            self.assertFalse(tool.covered(10, start, end, 6, 2, alive))

    def test_actual_subprocess_overlap_and_raw_capture(self):
        # The fake client verifies bulk is active at BOTH ends of connect.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / "perfload"
            fake.write_text('''#!/usr/bin/env python3
import json, sys, time
from pathlib import Path
marker = Path(__file__).with_suffix('.active')
mode = sys.argv[1]
duration = float(sys.argv[sys.argv.index('-d')+1][:-1])
if mode == 'bulk':
    marker.touch()
    time.sleep(duration)
    marker.unlink()
    data = dict(gbit_per_second=1, slowest_stream_gbit_per_second=1)
else:
    # Wait for fixture readiness rather than assuming interpreter startup
    # completes within a fraction of a second on a busy CI runner.
    deadline = time.monotonic() + 2
    while not marker.exists() and time.monotonic() < deadline:
        time.sleep(.01)
    assert marker.exists()
    time.sleep(duration)
    assert marker.exists()
    data = dict(ok=10, connections_per_second=10, latency_p50_ms=1, latency_p95_ms=1.5, latency_p99_ms=2)
print(json.dumps(dict(mode=mode, seconds=duration, errors={}, **data)))
''')
            fake.chmod(0o755)
            args = argparse.Namespace(output=root, perfload=str(fake), warmup=1,
                                      duration=.1, tail=3, timeout=1, streams=1,
                                      concurrency=1, src="")
            row = tool.run_trial(args, "baseline", "127.0.0.1:9000", 1, 0)
            self.assertTrue(row["valid"], row)
            self.assertTrue(row["timing"]["bulk_alive_after_connect"])
            for mode in ("bulk", "connect"):
                raw = Path(row["raw_prefix"] + f".{mode}.json")
                self.assertEqual(json.loads(raw.read_text())["mode"], mode)
            # A failing executable must preserve stderr and reject the run.
            fake.write_text('#!/bin/sh\necho deliberate-failure >&2\nexit 2\n')
            row = tool.run_trial(args, "baseline", "127.0.0.1:9000", 2, 0)
            self.assertFalse(row["valid"])
            self.assertIn("deliberate-failure", Path(row["raw_prefix"] + ".bulk.stderr").read_text())


if __name__ == "__main__":
    unittest.main()
