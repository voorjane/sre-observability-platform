#!/usr/bin/env python3
"""Run k6 or controlled fault injection against the running Compose stack."""

import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import signal
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
from smoke import ROOT, Smoke, SmokeFailure, require, write_json


def utc(timestamp=None):
    return datetime.fromtimestamp(time.time() if timestamp is None else timestamp, timezone.utc).isoformat()


class Experiment(Smoke):
    def __init__(self, args, mode):
        super().__init__(args, mode=mode)
        self.report.update(mode=mode, phases=[])
        del self.report["cases"]

    def prom(self, query):
        result = self.api("prometheus", "/api/v1/query", {"query": query})
        require(result["status"] == "success", f"Prometheus query failed: {query}")
        return result["data"]["result"]

    def scrape(self, after=0):
        rows = self.prom('timestamp(up{job="order-api"}) and (up{job="order-api"} == 1)')
        require(len(rows) == 1, "Expected one healthy order-api target")
        stamp = float(rows[0]["value"][1])
        require(stamp >= after, "Waiting for scrape after the phase")
        return stamp

    def measure(self, before, after):
        selector = 'service="order-api",method="POST",handler="/api/v1/orders"'
        counters = f"http_requests_total{{{selector}}}"
        buckets = f'http_request_duration_seconds_bucket{{{selector},status_code!~"4.."}}'

        def delta(metric):
            # Query just after each scrape to include the sample at the boundary.
            start = f"{metric} @ {before + 0.001:.3f}"
            end = f"{metric} @ {after + 0.001:.3f}"
            return f"(({end}) - (({start}) or (0 * ({end}))))"

        rows = self.prom(delta(counters))
        statuses = {row["metric"]["status_code"]: int(float(row["value"][1])) for row in rows}
        require(all(n >= 0 for n in statuses.values()), "Counter reset during measurement")
        statuses = {status: n for status, n in statuses.items() if n > 0}
        requests = sum(n for status, n in statuses.items() if not status.startswith("4"))
        errors = sum(n for status, n in statuses.items() if status.startswith("5"))
        quantile = self.prom(f"histogram_quantile(0.95, sum by (le) ({delta(buckets)}))")
        p95 = float(quantile[0]["value"][1]) if quantile else math.nan
        ratio = errors / requests if requests else None
        return {"first_scrape": utc(before), "last_scrape": utc(after),
                "statuses": statuses, "requests": requests, "errors": errors,
                "error_rate": ratio, "burn_rate": ratio / 0.001 if ratio is not None else None,
                "p95_ms": p95 * 1000 if math.isfinite(p95) else None}

    def phase_report(self, phase):
        self.report["phases"].append(phase)
        self.save_report()
        print(json.dumps(phase, ensure_ascii=False), flush=True)


class Load(Experiment):
    def __init__(self, args):
        super().__init__(args, "load")
        self.args = args
        self.report.update(profile=args.profile, k6_image="grafana/k6:2.3.0")

    def k6(self, name, rps, seconds, warmup=False):
        args = ["run", "--rm", "--no-deps", "--no-TTY",
                "-e", "BASE_URL=http://order-api:8000", "-e", f"LOAD_RPS={rps}",
                "-e", f"LOAD_SECONDS={seconds}", "-e", f"LOAD_PHASE={'warmup' if warmup else 'measure'}",
                "k6", "run", "--quiet", "--no-color", "/scenarios/orders.js"]
        log = self.output / f"{name}.log"
        started = time.time()
        with log.open("w") as stream:
            result = subprocess.run(self.command + args, cwd=ROOT, text=True,
                                    stdout=stream, stderr=subprocess.STDOUT, timeout=seconds + 180)
        ended = time.time()
        if result.returncode not in {0, 99}:
            raise subprocess.CalledProcessError(result.returncode, args)
        lines = log.read_text().splitlines()
        summaries = [line.partition("=")[2] for line in lines if line.startswith("K6_ORDER_SUMMARY=")]
        require(len(summaries) == 1, f"k6 exited {result.returncode} without a summary; see {log}")
        summary = json.loads(summaries[0])
        write_json(self.output / f"{name}-k6.json", summary)
        metrics = summary["metrics"]

        def values(metric):
            return metrics.get(metric, {}).get("values", {})

        phase = {"phase": name, "started_at": utc(started), "ended_at": utc(ended), "rps": rps,
                 "duration_seconds": seconds, "k6_exit": result.returncode,
                 "p95_ms": values("order_duration").get("p(95)"),
                 "requests": values("http_reqs").get("count", 0),
                 "error_rate": values("order_failure").get("rate"),
                 "transport_failures": values("order_transport_failures").get("count", 0),
                 "dropped_iterations": values("dropped_iterations").get("count", 0)}
        require(phase["requests"] > 0, "k6 sent no requests")
        return phase, ended

    def run(self):
        self.connect([("order-api", 8000, "/healthz"), ("prometheus", 9090, "/-/ready")])
        rates = self.args.rates
        if self.args.warmup:
            phase, ended = self.k6("warmup", rates[0], self.args.warmup, warmup=True)
            self.phase_report(phase)
            require(phase["error_rate"] == 0, "Warmup did not pass the order contract")
            self.wait("warmup captured by Prometheus", lambda: self.scrape(ended))
        failed = False
        for rps in rates:
            before = self.wait("pre-test scrape", self.scrape)
            phase, ended = self.k6(f"orders-{rps}", rps, self.args.seconds)
            after = self.wait("post-test scrape", lambda: self.scrape(ended))
            phase["prometheus"] = self.measure(before, after)
            self.phase_report(phase)
            failed |= phase["k6_exit"] != 0
        return 99 if failed else 0


def bounded(lo, hi):
    def validate(raw):
        try:
            n = int(raw)
        except ValueError as exc:
            raise argparse.ArgumentTypeError(f"expected an integer, got {raw!r}") from exc
        if not lo <= n <= hi:
            raise argparse.ArgumentTypeError(f"expected {lo}..{hi}, got {n}")
        return n
    return validate


def rates_from(raw):
    rates = [bounded(1, 1500)(part) for part in raw.split(",")]
    if not 2 <= len(rates) <= 6 or rates != sorted(set(rates)):
        raise argparse.ArgumentTypeError("specify 2–6 distinct increasing rates")
    return rates


def run_experiment(experiment):
    def interrupted(signum, _frame):
        # Further signals must not interrupt worker recovery or report writing.
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        raise SmokeFailure(f"Interrupted by signal {signum}")

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    status = 1
    try:
        status = experiment.run()
        experiment.report["status"] = "passed" if status == 0 else "thresholds_failed"
    except Exception as exc:
        if isinstance(exc, subprocess.CalledProcessError):
            status = exc.returncode if exc.returncode > 0 else 128 - exc.returncode
        experiment.report.update(status="failed", error=str(exc))
        print(f"Experiment failed: {exc}", file=sys.stderr, flush=True)
    finally:
        if experiment.report["status"] == "failed":
            experiment.diagnostics()
        experiment.report["finished_at"] = utc()
        experiment.save_report()
    print(f"Experiment {experiment.report['status']}; evidence: {experiment.output}", flush=True)
    return status


def parser_for(mode, description):
    parser = argparse.ArgumentParser(description=description)
    parser.add_argument("--compose", default="docker compose")
    parser.add_argument("--timeout", type=bounded(15, 300), default=90)
    parser.add_argument("--artifacts-dir", type=Path, default=ROOT / "artifacts" / mode)
    return parser


def main():
    parser = parser_for("load", __doc__)
    parser.add_argument("--profile", choices=("smoke", "baseline", "ramp"), default="baseline")
    parser.add_argument("--rps", type=bounded(1, 1500))
    parser.add_argument("--rates", type=rates_from)
    parser.add_argument("--seconds", type=bounded(1, 300))
    parser.add_argument("--warmup", type=bounded(0, 120))
    args = parser.parse_args()
    if (args.profile == "ramp" and args.rps is not None) or (args.profile != "ramp" and args.rates is not None):
        parser.error("use --rates with ramp and --rps with smoke or baseline")
    defaults = {"smoke": ([5], 10, 5), "baseline": ([30], 60, 15),
                "ramp": ([100, 300, 600, 1000], 20, 15)}
    rates, seconds, warmup = defaults[args.profile]
    args.rates = args.rates or ([args.rps] if args.rps else rates)
    args.seconds = args.seconds or seconds
    args.warmup = warmup if args.warmup is None else args.warmup
    return run_experiment(Load(args))


if __name__ == "__main__":
    sys.exit(main())
