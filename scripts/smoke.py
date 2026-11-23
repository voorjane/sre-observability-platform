#!/usr/bin/env python3
"""Exercise a disposable Compose stack using only the Python standard library."""

import argparse
import base64
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import http.client
import json
import math
import os
from pathlib import Path
import re
import secrets
import shlex
import signal
import subprocess
import sys
import threading
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


def all_panels(panels):
    for panel in panels:
        yield panel
        yield from all_panels(panel.get("panels", []))


class Smoke:
    def __init__(self, args, project_prefix="sre-smoke-"):
        self.timeout = args.timeout
        self.project = project_prefix + secrets.token_hex(6)
        self.output = args.artifacts_dir.resolve() / self.project
        self.output.mkdir(parents=True)
        self.env = dict(os.environ, COMPOSE_PROJECT_NAME=self.project,
                        PAYMENT_TIMEOUT="5s", PROCESS_DELAY="20ms",
                        OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:4318",
                        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="",
                        OTEL_EXPORTER_OTLP_TIMEOUT="1000",
                        OTEL_EXPORTER_OTLP_TRACES_TIMEOUT="1000",
                        OTEL_TRACES_SAMPLER_ARG="1.0",
                        GRAFANA_ADMIN_USER="smoke-admin",
                        GRAFANA_ADMIN_PASSWORD=secrets.token_urlsafe(24),
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

    def http(self, service, path, data=None, headers=None, params=None, method=None):
        url = self.urls[service] + path
        if params:
            url += "?" + parse.urlencode(params)
        headers = dict(headers or {})
        if service == "grafana":
            credentials = self.env["GRAFANA_ADMIN_USER"] + ":" + self.env["GRAFANA_ADMIN_PASSWORD"]
            headers["Authorization"] = "Basic " + base64.b64encode(credentials.encode()).decode()
        req = request.Request(url, data=data, headers=headers, method=method)
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
                              ("prometheus", 9090), ("jaeger", 16686), ("loki", 3100),
                              ("grafana", 3000), ("alertmanager", 9093), ("alert-webhook", 8080)]:
            self.resolve_port(service, port)
        self.report["endpoints"] = self.urls
        self.save_report()
        for service, path in [("order-api", "/healthz"), ("payment-worker", "/healthz"),
                              ("prometheus", "/-/ready"), ("jaeger", "/api/v3/services"),
                              ("loki", "/ready"), ("grafana", "/api/health"),
                              ("alertmanager", "/-/ready"), ("alert-webhook", "/healthz")]:
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

    def recording_rules(self):
        groups = self.api("prometheus", "/api/v1/rules")["data"]["groups"]
        slo = [group for group in groups if group["name"] == "order-slo"]
        require(len(slo) == 1 and len(slo[0]["rules"]) == 43, "SLO recording rules not loaded")
        require(all(rule.get("health") == "ok" and not rule.get("lastError")
                    for rule in slo[0]["rules"]), "SLO rule evaluation failed")
        extra = {g["name"]: g for g in groups if g["name"] in
                 {"order-burn-rate", "availability-alerts", "telemetry-alerts"}}
        require(set(extra) == {"order-burn-rate", "availability-alerts", "telemetry-alerts"},
                "Production alerting groups were not loaded")
        require(all(r.get("health") == "ok" and not r.get("lastError")
                    for g in extra.values() for r in g["rules"]), "Alerting rule evaluation failed")
        require(all(r.get("state") == "inactive" for r in extra["availability-alerts"]["rules"]),
                "Fresh history incorrectly fired a production SLO alert")
        values = {}
        for name in ["window_ready:bool", "availability:ratio", "budget_remaining:ratio"]:
            query = 'order:slo_' + name + '{window="30d"}'
            result = self.api("prometheus", "/api/v1/query", {"query": query})["data"]["result"]
            if name == "window_ready:bool":
                require(len(result) == 1 and float(result[0]["value"][1]) == 0,
                        "Fresh stack incorrectly qualifies as a complete 30d window")
            else:
                require(not result, "Fresh stack reports a confirmed 30d SLO or budget")
            values[name] = result
        return {"group": slo[0], "alerting_groups": extra, "fresh_window_checks": values}

    def grafana_sources(self):
        expected = {"prometheus": ("prometheus", "http://prometheus:9090"),
                    "loki": ("loki", "http://loki:3100"),
                    "jaeger": ("jaeger", "http://jaeger:16686")}
        sources = self.api("grafana", "/api/datasources")
        require({source["uid"] for source in sources} == set(expected), "Wrong provisioned datasource UIDs")
        health = {}
        for source in sources:
            uid = source["uid"]
            require((source["type"], source["url"]) == expected[uid], "Wrong datasource type or internal URL")
            require(source.get("readOnly") and source.get("access") == "proxy", "Datasource is not managed by provisioning")
            require(source.get("isDefault") == (uid == "prometheus"), "Wrong default datasource")
            health[uid] = self.api("grafana", f"/api/datasources/uid/{uid}/health")
            require(health[uid].get("status") == "OK", f"Unhealthy Grafana datasource: {uid}")
        return {"sources": sources, "health": health}

    def grafana_queries(self):
        trace = self.report["cases"][0]["trace_id"]
        prometheus = self.api("grafana", "/api/datasources/proxy/uid/prometheus/api/v1/query", {"query": "up"})
        require(prometheus.get("status") == "success" and prometheus["data"]["result"], "Prometheus proxy query failed")
        loki = self.api("grafana", "/api/datasources/proxy/uid/loki/loki/api/v1/query_range", {
            "query": '{service=~"order-api|payment-worker"} | json | trace_id="' + trace + '"',
            "since": "10m", "limit": 20,
        })
        rows = [json.loads(value[1]) for stream in loki["data"]["result"] for value in stream["values"]]
        require({row["service"] for row in rows} == {"order-api", "payment-worker"}, "Loki proxy query lost trace logs")
        payload = {"from": str(int(time.time() * 1000) - 600000), "to": str(int(time.time() * 1000)),
                   "queries": [{"refId": "A", "datasource": {"type": "jaeger", "uid": "jaeger"}, "query": trace}]}
        code, _, raw = self.http("grafana", "/api/ds/query", data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
        require(code == 200, f"Jaeger datasource query: HTTP {code}")
        result = json.loads(raw)["results"]["A"]
        require(not result.get("error") and result.get("frames") and trace in json.dumps(result),
                "Jaeger datasource did not return the requested trace")
        return {"prometheus": prometheus, "loki": loki, "jaeger": result}

    def provisioned_dashboard(self, uid):
        result = self.api("grafana", "/api/dashboards/uid/" + uid)
        require(result["meta"].get("provisioned") and
                result["meta"].get("folderUid") == "sre-observability",
                f"Dashboard {uid} was not provisioned in the expected folder")
        return result["dashboard"]

    def panel_values(self, dashboard, panel_id, service=".*", route=".*", at=None):
        panel = next(p for p in all_panels(dashboard["panels"]) if p["id"] == panel_id)
        values = []
        for target in panel["targets"]:
            query = (target["expr"].replace("${service:regex}", service)
                     .replace("${route:regex}", route).replace("$__rate_interval", "5m"))
            params = {"query": query}
            if at is not None:
                params["time"] = at
            result = self.api("grafana", "/api/datasources/proxy/uid/prometheus/api/v1/query", params)
            require(result.get("status") == "success", f"Dashboard query failed: {panel['title']}")
            rows = result["data"]["result"]
            require(all(math.isfinite(float(row["value"][1])) for row in rows),
                    f"Dashboard returned NaN/Inf: {panel['title']}")
            values.extend(rows)
        return values

    def golden_signals(self, dashboard):
        def baseline():
            values = {str(p["id"]): self.panel_values(dashboard, p["id"])
                      for p in all_panels(dashboard["panels"]) if p.get("datasource", {}).get("type") == "prometheus"}
            require(all(values[key] for key in ["1", "2", "3", "4", "13"]),
                    "Waiting for two samples of every observed HTTP series")
            return values

        body = json.dumps({"item_id": "dashboard-smoke", "amount": 49.99,
                           "customer_id": "dashboard-smoke"}).encode()

        def send(query, expected):
            code, _, _ = self.http("order-api", ORDER_ROUTE + query, data=body,
                                    headers={"Content-Type": "application/json"})
            require(code == expected, f"Dashboard exercise expected {expected}, got {code}")

        send("", 201)
        baseline_at = time.time()
        self.wait("dashboard baseline scrape", lambda: self.targets(baseline_at))
        before = self.wait("golden signals baseline", baseline)

        # Existing 500 series has already been scraped. These increments must be
        # visible to rate(), unlike a lazy counter's very first observation.
        for _ in range(3):
            send("?fail_rate=1", 500)
        stop = threading.Event()

        def slow_orders():
            while not stop.is_set():
                send("?latency_ms=1000", 201)

        observed_in_flight = 0
        print("Exercising dashboard with concurrent slow requests...", flush=True)
        try:
            with ThreadPoolExecutor(max_workers=2) as pool:
                futures = [pool.submit(slow_orders) for _ in range(2)]
                try:
                    deadline = time.monotonic() + 22  # Longer than one 15s scrape interval.
                    while time.monotonic() < deadline:
                        rows = self.panel_values(dashboard, 4)
                        observed_in_flight = max(observed_in_flight,
                                                sum(float(r["value"][1]) for r in rows))
                        time.sleep(0.5)
                finally:
                    stop.set()
                for future in futures:
                    future.result()
        finally:
            stop.set()
        require(observed_in_flight > 0, "In-flight panel did not observe concurrent requests")
        completed_at = time.time()
        self.wait("fresh dashboard samples", lambda: self.targets(completed_at))

        after = {str(p["id"]): self.panel_values(dashboard, p["id"])
                 for p in all_panels(dashboard["panels"]) if p.get("datasource", {}).get("type") == "prometheus"}
        value = lambda key: float(after[key][0]["value"][1])
        require(value("1") > float(before["1"][0]["value"][1]), "RPS did not increase")
        require(value("2") > 0, "5xx fraction did not reflect injected failures")
        require(value("3") > max(0.25, float(before["3"][0]["value"][1])),
                "p95 did not reflect injected latency")
        require({row["metric"]["service"] for row in after["5"]} ==
                {"order-api", "payment-worker"}, "Traffic breakdown lost a service")
        require(any(row["metric"].get("status_code") == "500" and float(row["value"][1]) > 0
                    for row in after["8"]), "HTTP status breakdown lost failures")
        require(all(after[key] for key in ["7", "9", "10", "11", "12"]),
                "Missing latency, in-flight or runtime/process series")
        require(len(after["13"]) == 2 and all(float(r["value"][1]) == 1 for r in after["13"]),
                "Scrape availability panel did not report both services UP")
        filtered = {}
        for service, route in [("order-api", ORDER_ROUTE), ("payment-worker", WORKER_ROUTE)]:
            rows = self.panel_values(dashboard, 5, service, route)
            require(rows and all(r["metric"].get("service") == service and
                                 r["metric"].get("handler") == route for r in rows),
                    "Service/route filters did not isolate traffic")
            filtered[service] = rows
        worker_errors = self.panel_values(dashboard, 2, "payment-worker", WORKER_ROUTE)
        require(worker_errors and float(worker_errors[0]["value"][1]) == 0,
                "Successful worker traffic must display a known 0% error ratio")
        for panel_id in [1, 2, 3, 5, 6, 7, 8]:
            require(not self.panel_values(dashboard, panel_id, route="/nonexistent-smoke-route"),
                    "Missing route was displayed as a measured zero")
        require(self.panel_values(dashboard, 4, route="/nonexistent-smoke-route"),
                "Route filter incorrectly affected the service-wide in-flight gauge")
        write_json(self.output / "golden-signals.json", {"uid": dashboard["uid"],
                   "before": before, "after": after, "filtered_traffic": filtered,
                   "worker_error_ratio": worker_errors, "observed_in_flight": observed_in_flight,
                   "missing_route_is_no_data": True})

    def slo_dashboard(self):
        dashboard = self.provisioned_dashboard("order-slo")
        at = time.time()
        values = {str(p["id"]): self.panel_values(dashboard, p["id"], at=at)
                  for p in all_panels(dashboard["panels"]) if p.get("datasource", {}).get("type") == "prometheus"}
        for key in ["31", "32", "33", "34"]:
            require(not values[key], "Fresh dashboard reports a confirmed 30d SLO or budget")
        number = lambda key: float(values[key][0]["value"][1])
        require(number("5") == 0, "Fresh dashboard incorrectly confirms 30d readiness")
        require(0 <= number("6") < 3600 and 0 <= number("7") < 1,
                "Fresh dashboard misrepresents history depth or coverage")
        require(all(values[key] for key in ["1", "3", "4", "10", "21", "22", "26", "27", "28"]),
                "Missing provisional SLI, budget, burn rate or latency fraction")
        counts = {}
        for window in ["5m", "1h", "6h", "30d"]:
            counts[window] = {}
            for name in ["requests", "errors"]:
                result = self.api("grafana", "/api/datasources/proxy/uid/prometheus/api/v1/query",
                                  {"query": f'order:slo_{name}:increase{{window="{window}"}}', "time": at})
                counts[window][name] = float(result["data"]["result"][0]["value"][1])
        ratios = {window: c["errors"] / c["requests"] for window, c in counts.items()}
        ratio = ratios["5m"]
        require(math.isclose(number("21"), ratio), "5m error panel disagrees with E/N from S3-02")
        require(0 < ratio < 1 and number("22") > 0.25, "SLO diagnostics lost injected failures/latency")
        require(math.isclose(number("1"), 1 - ratios["30d"]) and
                math.isclose(number("3"), 1 - ratios["30d"] / 0.001) and
                math.isclose(number("4"), ratios["30d"] / 0.001),
                "Provisional 30d availability or budget disagrees with observed E/N")
        require(number("3") < 0 and number("4") > 1, "Budget overrun was hidden or clamped")
        require(math.isclose(number("27"), ratios["1h"] / 0.001), "1h burn rate uses the wrong error budget")
        require({r["metric"]["window"] for r in values["28"]} == {"1h", "6h"} and
                all(math.isclose(float(r["value"][1]), ratios[r["metric"]["window"]] / 0.001)
                    for r in values["28"]), "Burn rate graph lost a window or changed its formula")
        buckets = {}
        for le in ["0.25", "+Inf"]:
            query = ('sum(increase(http_request_duration_seconds_bucket{service="order-api",method="POST",'
                     'handler="/api/v1/orders",status_code!~"4..",le="' + le + '"}[5m]))')
            result = self.api("grafana", "/api/datasources/proxy/uid/prometheus/api/v1/query", {"query": query, "time": at})
            buckets[le] = float(result["data"]["result"][0]["value"][1])
        require(math.isclose(number("26"), buckets["0.25"] / buckets["+Inf"]) and 0 <= number("26") < 0.95,
                "Latency fraction lost slow requests or disagrees with the histogram")
        panel = next(p for p in all_panels(dashboard["panels"]) if p["type"] == "logs")
        query = panel["targets"][0]["expr"].replace("${log_status:raw}", "5..")
        logs = self.api("grafana", "/api/datasources/proxy/uid/loki/loki/api/v1/query_range",
                        {"query": query, "since": "10m", "limit": 100})
        rows = [json.loads(v[1]) for stream in logs["data"]["result"] for v in stream["values"]]
        require(rows and all(r["service"] == "order-api" and r["route"] == ORDER_ROUTE and
                             r["status_code"] >= 500 for r in rows), "SLO log panel lost its error filter")
        return {"uid": dashboard["uid"], "panels": values, "counts": counts,
                "latency_buckets_5m": buckets, "error_logs": rows}

    def log_trace_links(self):
        source = self.api("grafana", "/api/datasources/uid/loki")
        field = next((f for f in source["jsonData"].get("derivedFields", []) if f["name"] == "TraceID"), None)
        require(field and field.get("datasourceUid") == "jaeger" and
                field.get("url") == "${__value.raw}", "Internal log-to-trace link was not provisioned")
        evidence = []
        for case in [self.report["cases"][0], self.report["cases"][2]]:
            trace = case["trace_id"]
            result = self.api("grafana", "/api/datasources/proxy/uid/loki/loki/api/v1/query_range",
                              {"query": '{service="order-api"} | json | trace_id="' + trace + '"',
                               "since": "10m", "limit": 20})
            lines = {v[1] for stream in result["data"]["result"] for v in stream["values"]}
            require(lines, "No source log for log-to-trace link")
            for line in lines:
                match = re.search(field["matcherRegex"], line)
                require(match and match.group(1) == trace, "Derived field extracted the wrong Trace ID")
            query = field["url"].replace("${__value.raw}", trace)
            payload = {"from": str(int(time.time() * 1000) - 600000), "to": str(int(time.time() * 1000)),
                       "queries": [{"refId": "A", "datasource": {"type": "jaeger", "uid": field["datasourceUid"]},
                                    "query": query}]}
            code, _, raw = self.http("grafana", "/api/ds/query", data=json.dumps(payload).encode(),
                                     headers={"Content-Type": "application/json"})
            require(code == 200, f"Derived trace query: HTTP {code}")
            result = json.loads(raw)["results"]["A"]
            require(not result.get("error") and result.get("frames") and trace in json.dumps(result),
                    "Derived link did not resolve its source trace")
            evidence.append({"case": case["name"], "trace_id": trace, "lines": sorted(lines), "trace": result})
        return {"derived_field": field, "cases": evidence}

    def grafana(self):
        before = self.wait("Grafana datasource provisioning and health", self.grafana_sources)
        queries = self.wait("Grafana datasource queries", self.grafana_queries)
        dashboard = self.wait("golden signals dashboard provisioning",
                              lambda: self.provisioned_dashboard("golden-signals"))
        self.golden_signals(dashboard)
        slo_before = self.wait("SLO dashboard and incomplete 30d history", self.slo_dashboard)
        links_before = self.wait("log-to-trace links", self.log_trace_links)
        preferences = {"theme": "light", "timezone": "utc"}
        code, _, _ = self.http("grafana", "/api/user/preferences", data=json.dumps(preferences).encode(),
                               headers={"Content-Type": "application/json"}, method="PUT")
        require(code == 200, "Cannot persist a Grafana user preference")
        old_id = self.compose("ps", "--quiet", "grafana").strip()
        self.compose("up", "--detach", "--no-deps", "--force-recreate", "grafana")
        require(self.compose("ps", "--quiet", "grafana").strip() != old_id, "Grafana was not recreated")
        self.resolve_port("grafana", 3000)
        self.wait("Grafana readiness after recreation", lambda: self.ready("grafana", "/api/health"))
        after = self.wait("Grafana datasource restoration", self.grafana_sources)
        require({s["uid"]: s["id"] for s in before["sources"]} == {s["uid"]: s["id"] for s in after["sources"]},
                "Grafana datasource IDs changed after recreation")
        restored = self.api("grafana", "/api/user/preferences")
        require(all(restored.get(key) == value for key, value in preferences.items()), "Grafana database was not preserved")
        self.wait("Grafana queries after recreation", self.grafana_queries)
        restored_dashboard = self.wait("golden signals dashboard restoration",
                                       lambda: self.provisioned_dashboard("golden-signals"))
        require(restored_dashboard["uid"] == dashboard["uid"] and
                self.panel_values(restored_dashboard, 1), "Dashboard failed after recreation")
        slo_after = self.wait("SLO dashboard restoration", self.slo_dashboard)
        links_after = self.wait("log-to-trace link restoration", self.log_trace_links)
        write_json(self.output / "slo-dashboard.json", {"before": slo_before, "after": slo_after})
        write_json(self.output / "log-trace.json", {"before": links_before, "after": links_after})
        write_json(self.output / "grafana.json", {"before": before, "after": after,
                   "queries": queries, "persisted_preferences": preferences,
                   "restored_dashboard_uid": restored_dashboard["uid"]})
        self.save_report()

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
            # Recreate so Docker allocates a fresh port instead of reusing a busy
            # rootless port mapping retained by the stopped container.
            self.compose("up", "--detach", "--no-deps", "--force-recreate", "payment-worker")
        # The replacement container has a new ephemeral host port.
        self.resolve_port("payment-worker", 8001)
        self.wait("worker recovery", lambda: self.ready("payment-worker", "/healthz"))
        self.order("recovery", 201)
        recovered_at = time.time()
        metrics = self.wait("fresh Prometheus samples", lambda: self.metrics(baseline, recovered_at))
        write_json(self.output / "prometheus.json", metrics)
        rules = self.wait("SLO recording rules and incomplete 30d history", self.recording_rules)
        write_json(self.output / "prometheus-rules.json", rules)
        series = self.api("loki", "/loki/api/v1/series", {"match[]": '{service=~"order-api|payment-worker"}'})["data"]
        require(sorted(series, key=lambda row: row["service"]) == [{"service": "order-api"}, {"service": "payment-worker"}],
                "Unexpected indexed Loki labels")
        self.grafana()

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
