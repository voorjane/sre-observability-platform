#!/usr/bin/env python3
"""Exercise a disposable Compose stack using only the Python standard library."""

import argparse
from collections import Counter
from datetime import datetime, timezone
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import signal
import subprocess
import sys
import time
from urllib import error, parse, request

ROOT = Path(__file__).resolve().parents[1]
ORDER_ROUTE = "/api/v1/orders"
WORKER_ROUTE = "/internal/process"
ORDER_SERVER = ("order-api", "POST " + ORDER_ROUTE, 2)
ORDER_OPERATION = ("order-api", "order.create", 1)
CLIENT = ("order-api", "POST " + WORKER_ROUTE, 3)
WORKER_SERVER = ("payment-worker", "POST " + WORKER_ROUTE, 2)
WORKER_OPERATION = ("payment-worker", "payment.process", 1)


class SmokeFailure(Exception):
    pass


def require(condition, message):
    if not condition:
        raise SmokeFailure(message)


def write_json(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


class Smoke:
    def __init__(self, args):
        self.timeout = args.timeout
        self.project = "sre-smoke-" + secrets.token_hex(6)
        self.output = args.artifacts_dir.resolve() / self.project
        self.output.mkdir(parents=True)
        self.env = dict(os.environ, COMPOSE_PROJECT_NAME=self.project,
                        PAYMENT_TIMEOUT="5s", PROCESS_DELAY="20ms",
                        OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:4318",
                        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="",
                        OTEL_EXPORTER_OTLP_TIMEOUT="1000",
                        OTEL_EXPORTER_OTLP_TRACES_TIMEOUT="1000",
                        OTEL_TRACES_SAMPLER_ARG="1.0",
                        LOKI_RETENTION_PERIOD="168h", PROMETHEUS_RETENTION_TIME="35d")
        self.command = shlex.split(args.compose) + [
            "--project-name", self.project, "--file", str(ROOT / "docker-compose.yml"),
            "--file", str(ROOT / "docker-compose.smoke.yml"),
        ]
        self.urls = {}
        self.report = {"project": self.project, "status": "running", "cases": [],
                       "started_at": datetime.now(timezone.utc).isoformat()}
        self.expected = Counter()
        self.started = False
        self.save_report()

    def save_report(self):
        write_json(self.output / "report.json", self.report)

    def compose(self, *args, timeout=60):
        result = subprocess.run(self.command + list(args), cwd=ROOT, env=self.env,
                                text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout)
        require(result.returncode == 0, f"Compose {' '.join(args)} failed:\n{result.stdout[-6000:]}")
        return result.stdout

    def wait(self, label, check):
        print(f"Checking {label}...", flush=True)
        deadline = time.monotonic() + self.timeout
        last_error = "no result"
        while time.monotonic() < deadline:
            try:
                return check()
            except (SmokeFailure, OSError, error.URLError,
                    http.client.HTTPException, json.JSONDecodeError) as exc:
                last_error = str(exc)
                time.sleep(0.5)
        raise SmokeFailure(f"Timed out checking {label}: {last_error}")

    def http(self, service, path, data=None, headers=None, params=None):
        url = self.urls[service] + path
        if params:
            url += "?" + parse.urlencode(params)
        req = request.Request(url, data=data, headers=headers or {})
        try:
            response = request.urlopen(req, timeout=5)
        except error.HTTPError as exc:
            response = exc
        with response:
            return response.status, response.headers, response.read().decode("utf-8")

    def api(self, service, path, params=None):
        code, _, body = self.http(service, path, params=params)
        require(code == 200, f"{service} {path}: HTTP {code}: {body[:200]}")
        return json.loads(body)

    def ready(self, service, path):
        code, _, _ = self.http(service, path)
        require(code == 200, f"{service} readiness: HTTP {code}")

    def start(self):
        print(f"Starting isolated project {self.project}; evidence: {self.output}", flush=True)
        self.compose("config", "--quiet")
        self.started = True  # Clean up partial starts and failed builds as well.
        self.compose("up", "--detach", "--build", timeout=480)
        for service, port in [("order-api", 8000), ("payment-worker", 8001),
                              ("prometheus", 9090), ("jaeger", 16686), ("loki", 3100)]:
            self.resolve_port(service, port)
        self.report["endpoints"] = self.urls
        self.save_report()
        for service, path in [("order-api", "/healthz"), ("payment-worker", "/healthz"),
                              ("prometheus", "/-/ready"), ("jaeger", "/api/v3/services"),
                              ("loki", "/ready")]:
            self.wait(service + " readiness", lambda s=service, p=path: self.ready(s, p))
        self.wait("Prometheus targets", self.targets)

    def resolve_port(self, service, port):
        address = self.compose("port", service, str(port)).strip()
        require(re.fullmatch(r"127\.0\.0\.1:\d+", address), f"Unexpected binding: {address}")
        self.urls[service] = "http://" + address

    def targets(self, scraped_after=0):
        targets = self.api("prometheus", "/api/v1/targets")["data"]["activeTargets"]
        apps = [t for t in targets if t["labels"].get("job") in {"order-api", "payment-worker"}]
        require({t["labels"]["job"] for t in apps} == {"order-api", "payment-worker"},
                "Missing application targets")
        for target in apps:
            require(target["health"] == "up", f"Target is not UP: {target['labels']}")
            scraped = datetime.fromisoformat(target["lastScrape"].replace("Z", "+00:00"))
            require(scraped.timestamp() >= scraped_after, "Waiting for a fresh scrape after recovery")
        return apps

    def baseline(self):
        code, _, body = self.http("order-api", "/metrics")
        require(code == 200, "Cannot read metric baseline")
        values = {}
        for line in body.splitlines():
            match = re.match(r"(http_requests_total|http_request_duration_seconds_sum)\{([^}]+)\} (\S+)", line)
            if match:
                name, raw_labels, value = match.groups()
                labels = dict(re.findall(r'(\w+)="([^"]*)"', raw_labels))
                if labels.get("handler") == ORDER_ROUTE and labels.get("method") == "POST":
                    values[(name, labels["status_code"])] = float(value)
        return values

    def order(self, name, status, query="", body=None, category=None, delay_ms=0):
        trace_id, parent_id = secrets.token_hex(16), secrets.token_hex(8)
        started = time.monotonic()
        code, headers, raw = self.http("order-api", ORDER_ROUTE + query,
            data=json.dumps(body if body is not None else {
                "item_id": "smoke-private-item", "amount": 49.99,
                "customer_id": "smoke-private-customer",
            }).encode(), headers={"Content-Type": "application/json",
                "traceparent": f"00-{trace_id}-{parent_id}-01"})
        case = {"name": name, "trace_id": trace_id, "parent_id": parent_id,
                "status_code": code, "duration_ms": (time.monotonic() - started) * 1000}
        self.report["cases"].append(case)
        self.save_report()
        require(code == status, f"{name}: expected HTTP {status}, got {code}: {raw[:200]}")
        require(headers.get_content_type() == "application/json", f"{name}: non-JSON response")
        response = json.loads(raw)
        if code == 201:
            require(response.get("status") == "COMPLETED" and response.get("payment_status") == "PROCESSED",
                    f"{name}: invalid success contract")
        else:
            require(isinstance(response.get("error"), str), f"{name}: invalid error contract")
        require(case["duration_ms"] >= delay_ms, f"{name}: latency injection was not applied")
        self.expected[str(code)] += 1
        spans = self.wait(name + " Jaeger trace", lambda: self.trace(case, category, delay_ms))
        logs = self.wait(name + " Loki logs", lambda: self.logs(case, spans, category, delay_ms))
        write_json(self.output / "traces" / (name + ".json"), list(spans.values()))
        write_json(self.output / "logs" / (name + ".json"), logs)
        case["spans"] = len(spans)
        case["logs"] = len(logs)
        self.save_report()

    def trace(self, case, category, delay_ms):
        code, _, raw = self.http("jaeger", "/api/v3/traces/" + case["trace_id"])
        require(code == 200, f"Trace not available: HTTP {code}")
        records = [json.loads(line) for line in raw.splitlines() if line.strip()]
        spans = {}
        for record in records:
            require("error" not in record, f"Jaeger: {record.get('error')}")
            for resource in record.get("result", {}).get("resourceSpans", []):
                attrs = {a["key"]: a["value"].get("stringValue") for a in resource["resource"]["attributes"]}
                for scope in resource.get("scopeSpans", []):
                    for span in scope.get("spans", []):
                        key = (attrs.get("service.name"), span["name"], span["kind"])
                        require(key not in spans, f"Duplicate span role: {key}")
                        require(span["traceId"] == case["trace_id"], "Trace ID mismatch")
                        spans[key] = span
        wanted = {ORDER_SERVER}
        if case["status_code"] in {201, 503}:
            wanted |= {ORDER_OPERATION, CLIENT}
        if case["status_code"] == 201:
            wanted |= {WORKER_SERVER, WORKER_OPERATION}
        require(set(spans) == wanted, f"Expected {len(wanted)} spans, found {list(spans)}")
        root = spans[ORDER_SERVER]
        require(root["parentSpanId"] == case["parent_id"], "Incoming parent was lost")
        for child, parent in [(ORDER_OPERATION, ORDER_SERVER), (CLIENT, ORDER_OPERATION),
                              (WORKER_SERVER, CLIENT), (WORKER_OPERATION, WORKER_SERVER)]:
            if child in spans:
                require(spans[child]["parentSpanId"] == spans[parent]["spanId"], f"Broken parent link: {child}")
        if case["status_code"] >= 500:
            require(root.get("status", {}).get("code") == 2, "Server error missing from trace")
        if case["status_code"] == 503:
            require(spans[CLIENT].get("status", {}).get("message") == category,
                    "Dependency error category missing from client span")
        duration = (int(root["endTimeUnixNano"]) - int(root["startTimeUnixNano"])) / 1e6
        require(duration >= delay_ms, "Server span excluded injected latency")
        require("smoke-private-" not in json.dumps(list(spans.values())), "Private request data in spans")
        return spans

    def logs(self, case, spans, category, delay_ms):
        data = self.api("loki", "/loki/api/v1/query_range", {
            "query": '{service=~"order-api|payment-worker"} | json | trace_id="' + case["trace_id"] + '"',
            "since": "10m", "limit": 100,
        })
        # Loki can replay entries after a reconnect; compare unique source records.
        lines = {value[1] for stream in data["data"]["result"] for value in stream["values"]}
        rows = [json.loads(line) for line in lines]
        expected = {"order-api": case["status_code"]}
        if case["status_code"] == 201:
            expected["payment-worker"] = 200
        require(len(rows) == len(expected), f"Expected {len(expected)} logs, found {len(rows)}")
        require({row.get("service") for row in rows} == set(expected), "Missing service log")
        for row in rows:
            service, code = row["service"], expected[row["service"]]
            role = ORDER_SERVER if service == "order-api" else WORKER_SERVER
            require(row.get("event") == "http_request" and row.get("method") == "POST", "Wrong log event")
            require(row.get("trace_id") == case["trace_id"] and row.get("span_id") == spans[role]["spanId"],
                    "Log does not identify the Jaeger server span")
            require(row.get("route") == (ORDER_ROUTE if service == "order-api" else WORKER_ROUTE), "Wrong log route")
            require(row.get("status_code") == code, "Wrong logged response status")
            require(row.get("level") == ("error" if code >= 500 else "warn" if code >= 400 else "info"), "Wrong log level")
            require(row.get("duration_ms", -1) >= (delay_ms if service == "order-api" else 0), "Wrong log duration")
            if service == "order-api" and category:
                require(row.get("error.type") == category, "Wrong request error category")
        require("smoke-private-" not in json.dumps(rows), "Private request data in logs")
        return rows

    def metrics(self, baseline, scraped_after):
        targets = self.targets(scraped_after)
        values = {}
        for code, count in self.expected.items():
            query = 'http_requests_total{service="order-api",method="POST",handler="' + ORDER_ROUTE + '",status_code="' + code + '"}'
            result = self.api("prometheus", "/api/v1/query", {"query": query})["data"]["result"]
            value = sum(float(row["value"][1]) for row in result)
            require(value >= baseline.get(("http_requests_total", code), 0) + count,
                    f"Counter {code} did not increase by {count}: {value}")
            values[code] = value
        for query, minimum in [
            ('http_request_duration_seconds_sum{service="order-api",method="POST",handler="' + ORDER_ROUTE + '",status_code="201"}',
             baseline.get(("http_request_duration_seconds_sum", "201"), 0) + 0.25),
            ('http_requests_total{service="payment-worker",method="POST",handler="' + WORKER_ROUTE + '",status_code="200"}', 1),
        ]:
            result = self.api("prometheus", "/api/v1/query", {"query": query})["data"]["result"]
            require(sum(float(row["value"][1]) for row in result) >= minimum, "Missing latency or worker metric")
        return {"targets": targets, "order_counters": values}

    def run(self):
        self.start()
        baseline = self.baseline()
        self.order("success", 201)
        self.order("invalid", 400, body={}, category="invalid_request_fields")
        self.order("chaos", 500, query="?fail_rate=1", category="chaos_injected_failure")
        self.order("latency", 201, query="?latency_ms=250&private=smoke-private-query", delay_ms=250)
        try:
            self.compose("stop", "payment-worker")
            self.order("unavailable", 503, category="dependency_unreachable")
        finally:
            self.compose("start", "payment-worker")
        # Docker may allocate a different ephemeral host port on container restart.
        self.resolve_port("payment-worker", 8001)
        self.wait("worker recovery", lambda: self.ready("payment-worker", "/healthz"))
        self.order("recovery", 201)
        recovered_at = time.time()
        metrics = self.wait("fresh Prometheus samples", lambda: self.metrics(baseline, recovered_at))
        write_json(self.output / "prometheus.json", metrics)
        series = self.api("loki", "/loki/api/v1/series", {"match[]": '{service=~"order-api|payment-worker"}'})["data"]
        require(sorted(series, key=lambda row: row["service"]) == [{"service": "order-api"}, {"service": "payment-worker"}],
                "Unexpected indexed Loki labels")

    def diagnostics(self):
        for filename, command in [("compose-ps.txt", ["ps", "--all"]),
                                  ("compose-logs.txt", ["logs", "--no-color", "--timestamps", "--tail=1000"])]:
            try:
                data = self.compose(*command, timeout=20)
            except (SmokeFailure, OSError, subprocess.TimeoutExpired) as exc:
                data = str(exc)
            (self.output / filename).write_text(data, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compose", default="docker compose", help="Compose command, e.g. 'docker compose'")
    parser.add_argument("--timeout", type=float, default=90, help="Polling deadline per check, seconds")
    parser.add_argument("--artifacts-dir", type=Path, default=ROOT / "artifacts" / "smoke")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    smoke = Smoke(args)

    def interrupted(signum, _frame):
        raise SmokeFailure(f"Interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        smoke.run()
        smoke.report["status"] = "passed"
    except Exception as exc:
        smoke.report.update(status="failed", error=str(exc))
        print(f"Smoke failed: {exc}", file=sys.stderr, flush=True)
    finally:
        if smoke.report["status"] != "passed":
            smoke.diagnostics()
        if smoke.started:
            try:
                smoke.compose("down", "--volumes", "--remove-orphans", timeout=90)
                smoke.report["cleanup"] = "passed"
            except (SmokeFailure, OSError, subprocess.TimeoutExpired) as exc:
                smoke.report.update(status="failed", cleanup=str(exc))
                smoke.diagnostics()
        smoke.report["finished_at"] = datetime.now(timezone.utc).isoformat()
        smoke.save_report()
    print(f"Smoke {smoke.report['status']}; evidence: {smoke.output}", flush=True)
    return 0 if smoke.report["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
