#!/usr/bin/env python3
"""Repeat perfload connect under concurrent bulk load; standard library only."""
import argparse
import json
import math
from pathlib import Path
import statistics
import subprocess
import time


METRICS = ("gbit_per_second", "connections_per_second", "latency_p50_ms", "latency_p95_ms", "latency_p99_ms")


def positive(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value > 0


def validate(data, mode):
    if not isinstance(data, dict):
        raise ValueError(f"invalid {mode} JSON: expected object")
    if data.get("mode") != mode or not positive(data.get("seconds")):
        raise ValueError(f"invalid {mode} result")
    errors = data.get("errors")
    if not isinstance(errors, dict) or any(type(v) is not int or v != 0 for v in errors.values()):
        raise ValueError(f"{mode} errors: {errors!r}")
    keys = ("gbit_per_second", "slowest_stream_gbit_per_second") if mode == "bulk" else (
        "ok", "connections_per_second", "latency_p50_ms", "latency_p95_ms", "latency_p99_ms")
    if not all(positive(data.get(k)) for k in keys):
        raise ValueError(f"missing/zero/nonfinite {mode} measurements")
    if mode == "connect" and not (data["latency_p50_ms"] <= data["latency_p95_ms"] <= data["latency_p99_ms"]):
        raise ValueError("unordered percentiles")


def covered(bulk_start, connect_start, connect_end, bulk_duration, warmup, bulk_alive):
    # Launch precedes perfload's internal clock, so launch+duration is a
    # conservative deadline. Do not use process exit: output/cleanup may lag.
    return (bulk_alive and connect_start >= bulk_start + warmup
            and connect_end < bulk_start + bulk_duration)


def summarize(rows, paired):
    valid = [r for r in rows if r["valid"]]
    result = {"valid_runs": len(valid), "rejected_runs": len(rows) - len(valid)}
    result["target_medians"] = {
        label: {key: statistics.median(r["metrics"][key] for r in valid if r["label"] == label)
                for key in METRICS}
        for label in sorted({r["label"] for r in valid})}
    if paired:
        pairs = []
        for index in sorted({r["round"] for r in rows}):
            group = {r["label"]: r for r in valid if r["round"] == index}
            if "baseline" in group and "candidate" in group:
                pairs.append({"round": index, **{
                    key: group["candidate"]["metrics"][key] / group["baseline"]["metrics"][key]
                    for key in METRICS}})
        result["paired_candidate_over_baseline"] = pairs
        result["paired_ratio_medians"] = {
            key: statistics.median(p[key] for p in pairs) for key in METRICS} if pairs else {}
    return result


def run_trial(args, label, target, index, order):
    prefix = args.output / f"{index:03d}-{order}-{label}"
    row = {"round": index, "label": label, "target": target, "valid": False,
           "raw_prefix": str(prefix)}
    processes, files = [], []
    duration = args.warmup + args.duration + args.tail

    def launch(mode, seconds):
        cmd = [args.perfload, mode, "-target", target, "-d", f"{seconds}s"]
        cmd += ["-streams", str(args.streams)] if mode == "bulk" else [
            "-c", str(args.concurrency), "-timeout", f"{args.timeout}s"]
        if args.src:
            cmd += ["-src", args.src]
        row[mode + "_command"] = cmd
        out = open(str(prefix) + f".{mode}.json", "w")
        err = open(str(prefix) + f".{mode}.stderr", "w")
        files.extend([out, err])
        started = time.monotonic()
        proc = subprocess.Popen(cmd, stdout=out, stderr=err)
        processes.append(proc)
        return proc, started

    try:
        bulk, start = launch("bulk", duration)
        time.sleep(args.warmup)
        if bulk.poll() is not None:
            raise ValueError("bulk exited during warmup")
        connect, conn_start = launch("connect", args.duration)
        connect.wait(timeout=args.duration + 2 * args.timeout + args.tail)
        conn_end = time.monotonic()
        alive = bulk.poll() is None
        row["timing"] = {"connect_start_after_bulk_launch_s": conn_start - start,
                         "connect_end_after_bulk_launch_s": conn_end - start,
                         "bulk_requested_s": duration, "bulk_alive_after_connect": alive}
        coverage = covered(start, conn_start, conn_end, duration, args.warmup, alive)
        bulk.wait(timeout=max(0, start + duration - time.monotonic()) + 10)
        for mode, proc in (("bulk", bulk), ("connect", connect)):
            row[mode + "_exit"] = proc.returncode
            if proc.returncode:
                raise ValueError(f"{mode} exit {proc.returncode}")
            data = json.loads(Path(str(prefix) + f".{mode}.json").read_text())
            row[mode] = data
            validate(data, mode)
        if not coverage:
            raise ValueError("bulk did not conservatively cover the entire connect process")
        row["metrics"] = {key: row["bulk" if key == "gbit_per_second" else "connect"][key]
                          for key in METRICS}
        row["valid"] = True
    except (ValueError, OSError, subprocess.TimeoutExpired) as exc:
        row["rejection"] = str(exc)
    finally:
        for proc in processes:
            if proc.poll() is None:
                proc.kill()
            proc.wait()
        for file in files:
            file.close()
        Path(str(prefix) + ".run.json").write_text(json.dumps(row, indent=2) + "\n")
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--perfload", required=True, help="path to built perfload executable")
    parser.add_argument("--baseline", default="127.0.0.1:9000")
    parser.add_argument("--candidate", help="optional second target; does NOT configure algorithm versions")
    parser.add_argument("--output", type=Path, required=True, help="new output directory (must not exist)")
    parser.add_argument("--rounds", type=int, default=4)
    parser.add_argument("--streams", type=int, default=8)
    parser.add_argument("--concurrency", type=int, default=32)
    parser.add_argument("--warmup", type=float, default=7)
    parser.add_argument("--duration", type=float, default=8)
    parser.add_argument("--tail", type=float, default=8, help="bulk tail; allow two connection timeouts plus scheduling margin")
    parser.add_argument("--timeout", type=float, default=3)
    parser.add_argument("--cooldown", type=float, default=1)
    parser.add_argument("--src", default="", help="perfload source IP list for both workloads")
    args = parser.parse_args()
    for key in ("rounds", "streams", "concurrency", "warmup", "duration", "tail", "timeout"):
        if not positive(getattr(args, key)):
            parser.error(f"--{key} must be finite and positive")
    if not math.isfinite(args.cooldown) or args.cooldown < 0:
        parser.error("--cooldown must be finite and nonnegative")
    args.output.mkdir(parents=True, exist_ok=False)
    rows = []
    config = {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()}
    for index in range(1, args.rounds + 1):
        targets = [("baseline", args.baseline)]
        if args.candidate:
            targets.append(("candidate", args.candidate))
            if index % 2 == 0:
                targets.reverse()
        for order, (label, target) in enumerate(targets):
            row = run_trial(args, label, target, index, order)
            rows.append(row)
            print(json.dumps(row), flush=True)
            report = {"config": config, "runs": rows, "summary": summarize(rows, bool(args.candidate))}
            (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            time.sleep(args.cooldown)
    print(json.dumps(report["summary"], indent=2))
    return 0 if all(r["valid"] for r in rows) else 1


if __name__ == "__main__":
    raise SystemExit(main())
