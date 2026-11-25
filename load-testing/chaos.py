#!/usr/bin/env python3
"""Exercise order failures, order latency and dependency loss on the running Compose stack."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import math
import secrets
import sys
import time

from run import Experiment, bounded, parser_for, require, run_experiment, utc, write_json

PAYLOAD = b'{"item_id":"chaos-item","customer_id":"chaos-customer","amount":49.99}'


class Chaos(Experiment):
    def __init__(self, args):
        super().__init__(args, "chaos")
        self.args = args
        self.report.update(scenario=args.scenario, rps=args.rps, fail_rate=args.fail_rate,
                           latency_ms=args.latency_ms, timeline=[], notifications=[])
    def mark(self, event):
        entry = {"at": utc(), "event": event}
        self.report["timeline"].append(entry)
        self.save_report()
        print(f"{entry['at']} {event}", flush=True)

    def order_request(self, query):
        trace_id, parent_id = secrets.token_hex(16), secrets.token_hex(8)
        start = time.monotonic()
        code, _, body = self.http("order-api", "/api/v1/orders" + query, data=PAYLOAD, headers={
            "Content-Type": "application/json", "traceparent": f"00-{trace_id}-{parent_id}-01",
        })
        duration_ms = 1000 * (time.monotonic() - start)
        data = json.loads(body)
        if code == 201:
            require(data.get("status") == "COMPLETED" and data.get("payment_status") == "PROCESSED"
                    and data.get("order_id"), "Invalid successful order")
        else:
            require(isinstance(data.get("error"), str), "Invalid error body")
        return {"status_code": code, "duration_ms": duration_ms, "trace_id": trace_id, "parent_id": parent_id}

    def phase(self, name, seconds, codes=(201,), query=""):
        before = self.wait("pre-phase scrape", self.scrape)
        samples = []
        self.mark(name + " started")
        started = time.time()
        next_request = time.monotonic()
        deadline = next_request + seconds
        # Allow up to two seconds of overlapping work at the maximum rate.
        limit = max(16, 2 * self.args.rps + 4)
        with ThreadPoolExecutor(max_workers=limit) as pool:
            pending = set()
            while next_request < deadline:
                time.sleep(max(0, next_request - time.monotonic()))
                completed = {job for job in pending if job.done()}
                samples.extend(job.result() for job in completed)
                pending -= completed
                require(len(pending) < limit, "Traffic generator exceeded its concurrency limit")
                pending.add(pool.submit(self.order_request, query))
                # Keep the target spacing without bunching requests after a late wake-up.
                next_request = max(next_request + 1 / self.args.rps, time.monotonic())
            samples.extend(job.result() for job in pending)
        ended = time.time()
        after = self.wait("post-phase scrape", lambda: self.scrape(ended))
        prometheus = self.measure(before, after)
        statuses = {}
        for sample in samples:
            code = str(sample["status_code"])
            statuses[code] = statuses.get(code, 0) + 1
        require(len(samples) >= 0.9 * self.args.rps * seconds, f"{name}: traffic generator missed the target rate")
        p95 = sorted(s["duration_ms"] for s in samples)[math.ceil(0.95 * len(samples)) - 1]
        error_ratio = sum(n for code, n in statuses.items() if code.startswith("5")) / len(samples)
        self.phase_report({"phase": name, "started_at": utc(started), "ended_at": utc(ended),
                           "requests": len(samples), "statuses": statuses, "p95_ms": p95,
                           "error_rate": error_ratio, "burn_rate": error_ratio / 0.001,
                           "prometheus": prometheus})
        require(set(int(code) for code in statuses).issubset(codes), f"{name}: unexpected HTTP statuses {statuses}")
        return samples, error_ratio, p95

    def journal(self, after=0):
        return self.api("alert-webhook", "/events", {"after": after})

    def trace_evidence(self, phase, samples, code, category=None, delay_ms=0):
        sample = next((s for s in samples if s["status_code"] == code), None)
        require(sample is not None, f"{phase}: no HTTP {code} order")
        spans = self.wait(phase + " trace", lambda: self.trace(sample, category, delay_ms))
        logs = self.wait(phase + " logs", lambda: self.logs(sample, spans, category, delay_ms))
        write_json(self.output / f"{phase}-evidence.json", {
            "request": sample, "trace": list(spans.values()), "logs": logs,
        })
        self.mark(f"{phase} log/trace {sample['trace_id']}")

    def errors(self):
        self.phase("errors-baseline", 6)
        start = time.time()
        samples, error_ratio, _ = self.phase("errors-injection", self.args.seconds, (201, 500),
                                             f"?fail_rate={self.args.fail_rate}")
        self.phase("errors-recovery", 6)
        require(error_ratio > 0, "No injected errors observed; increase rate or duration")
        data = self.api("prometheus", "/api/v1/query_range", {
            "query": 'order:slo_burn_rate:observed', "start": start - 15, "end": time.time(), "step": "15s",
        })
        write_json(self.output / "burn-rate.json", data)
        self.trace_evidence("errors", samples, 500, "chaos_injected_failure")

    def latency(self):
        self.phase("latency-baseline", 6)
        samples, _, p95 = self.phase("latency-injection", self.args.seconds,
                                     query=f"?latency_ms={self.args.latency_ms}")
        _, _, recovered = self.phase("latency-recovery", 6)
        require(p95 >= 250, "Expected client p95 at or above 250 ms with no 5xx")
        require(recovered < 250, "Client p95 did not recover")
        self.trace_evidence("latency", samples, 201, delay_ms=self.args.latency_ms)

    def worker(self):
        self.phase("worker-baseline", 6)
        try:
            self.mark("payment-worker stop requested")
            self.compose("stop", "payment-worker")
            samples, _, _ = self.phase("worker-injection", self.args.seconds, (503,))
        finally:
            self.compose("start", "payment-worker")
            self.resolve_port("payment-worker", 8001)
            self.wait("worker readiness after restart", lambda: self.ready("payment-worker", "/healthz"))
            self.mark("payment-worker restored")
        self.phase("worker-recovery", 6)
        self.trace_evidence("worker", samples, 503, "dependency_unreachable")

    def run(self):
        self.connect([
            ("order-api", 8000, "/healthz"), ("payment-worker", 8001, "/healthz"),
            ("prometheus", 9090, "/-/ready"), ("jaeger", 16686, "/api/v3/services"),
            ("loki", 3100, "/ready"), ("alert-webhook", 8080, "/healthz"),
        ])
        cursor = self.journal()["last_id"]
        steps = {"errors": self.errors, "latency": self.latency, "worker": self.worker}
        try:
            for scenario, execute in steps.items():
                if self.args.scenario in {"all", scenario}:
                    execute()
        finally:
            journal = self.journal(cursor)
            write_json(self.output / "webhook.json", journal)
            self.report["notifications"] = journal["events"]
        return 0


def probability(raw):
    try:
        value = float(raw)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("expected a number in (0, 1]") from exc
    if not 0 < value <= 1:
        raise argparse.ArgumentTypeError("expected a number in (0, 1]")
    return value


def main():
    parser = parser_for("chaos", __doc__)
    parser.add_argument("--scenario", choices=("all", "errors", "latency", "worker"), default="all")
    parser.add_argument("--rps", type=bounded(5, 30), default=10)
    parser.add_argument("--seconds", type=bounded(15, 120), default=25)
    parser.add_argument("--fail-rate", type=probability, default=1.0)
    parser.add_argument("--latency-ms", type=bounded(250, 2000), default=350)
    return run_experiment(Chaos(parser.parse_args()))


if __name__ == "__main__":
    sys.exit(main())
