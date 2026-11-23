#!/usr/bin/env python3
"""Validate the shipped dashboard queries and SLO display semantics with promtool."""

import argparse
import json
from pathlib import Path
import shlex
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def all_panels(panels):
    for panel in panels:
        yield panel
        yield from all_panels(panel.get("panels", []))


def validate_positions(panels, path):
    # Collapsed rows have their own layout; Grafana moves subsequent rows on expansion.
    for i, panel in enumerate(panels):
        a = panel["gridPos"]
        for other in panels[i + 1:]:
            b = other["gridPos"]
            assert not (a["x"] < b["x"] + b["w"] and b["x"] < a["x"] + a["w"] and
                        a["y"] < b["y"] + b["h"] and b["y"] < a["y"] + a["h"]), path
        validate_positions(panel.get("panels", []), path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compose", default="docker compose")
    args = parser.parse_args()
    expressions = []
    slo = None
    uids = set()
    for path in sorted((ROOT / "monitoring/grafana/dashboards").glob("*.json")):
        dashboard = json.loads(path.read_text())
        assert dashboard["uid"] not in uids, f"Duplicate dashboard UID: {path}"
        uids.add(dashboard["uid"])
        assert dashboard["refresh"] in dashboard["timepicker"]["refresh_intervals"], path
        panels = list(all_panels(dashboard["panels"]))
        ids = [p["id"] for p in panels]
        assert len(ids) == len(set(ids)), f"Duplicate panel ID: {path}"
        validate_positions(dashboard["panels"], path)
        for panel in panels:
            for target in panel.get("targets", []):
                if target["datasource"]["type"] == "prometheus":
                    expr = (target["expr"].replace("${service:regex}", ".*")
                            .replace("${route:regex}", ".*").replace("$__rate_interval", "5m"))
                    expressions.append({"record": f"dashboard:query_{len(expressions)}", "expr": expr})
        if dashboard["uid"] == "order-slo":
            slo = dashboard
    assert slo is not None, "Missing order-slo dashboard"
    queries = {(p["id"], t["refId"]): t["expr"] for p in all_panels(slo["panels"])
               for t in p.get("targets", []) if t["datasource"]["type"] == "prometheus"}

    def series(name, window, value):
        return {"series": f'order:slo_{name}{{window="{window}"}}', "values": f"{value}+0x20"}

    def fixture(name, ready=1, traffic=True, up=1, stale=False, new_bucket=False):
        inputs = [{"series": 'up{job="order-api",instance="a"}',
                   "values": f"{up}+0x15" if stale else f"{up}+0x20"}]
        for window in ["30d", "5m"]:
            inputs.append(series("window_ready:bool", window, ready if window == "30d" else 1))
        inputs.append(series("scrape_coverage:ratio", "30d", 0.01 if not ready else 1))
        if traffic:
            for metric, value in [("availability:ratio", 0.9988), ("budget_remaining:ratio", -0.2),
                                  ("latency_p95_seconds:quantile", 0.3), ("error_ratio:ratio", 0.0012)]:
                inputs.append(series(metric, "30d", value))
            for metric, value in [("availability:observed", 0.9988), ("budget_remaining:observed", -0.2),
                                  ("latency_p95_seconds:observed", 0.3)]:
                inputs.append(series(metric, "30d", value))
            for metric, value in [("availability:observed", 0.998), ("error_ratio:observed", 0.002),
                                  ("latency_p95_seconds:observed", 0.3)]:
                inputs.append(series(metric, "5m", value))
            inputs.append(series("error_ratio:observed", "1h", 0.002))
            inputs.append(series("error_ratio:observed", "6h", 0.0005))
        for le, increment in [("0.25", 90), ("+Inf", 100)]:
            values = f"0+{increment if traffic else 0}x20"
            if new_bucket and le == "0.25":
                values = " ".join(["_"] * 20 + ["90"])
            inputs.append({"series": 'http_request_duration_seconds_bucket{service="order-api",'
                           'method="POST",handler="/api/v1/orders",status_code="201",le="' + le + '"}',
                           "values": values})
            # A large excluded population must not dilute the latency fraction.
            for service, status in [("order-api", "400"), ("payment-worker", "200")]:
                inputs.append({"series": 'http_request_duration_seconds_bucket{service="' + service + '",'
                               'method="POST",handler="/api/v1/orders",status_code="' + status + '",le="' + le + '"}',
                               "values": "0+10000x20"})
        full = traffic and ready == 1 and up == 1 and not stale
        short = traffic and up == 1 and not stale
        expected = {1: 0.9988 if short else None, 3: -0.2 if short else None,
                    4: 1.2 if short else None, 5: ready * up if not stale else None,
                    6: 300, 8: 0.9988 if short else None, 9: -0.2 if short else None,
                    10: 0.3 if short else None, 11: up if not stale else None,
                    21: 0.002 if short else None, 22: 0.3 if short else None,
                    26: 0.9 if short and not new_bucket else None, 27: 2 if short else None,
                    (28, "A"): 2 if short else None, (28, "B"): 0.5 if short else None,
                    31: 0.9988 if full else None, 32: -0.2 if full else None,
                    33: 1.2 if full else None, 34: 0.3 if full else None}
        return {"name": name, "interval": "15s", "input_series": inputs,
                "promql_expr_test": [{"expr": f"round(sum({queries[key if isinstance(key, tuple) else (key, 'A')]}), 0.000000001)",
                                      "eval_time": "5m", "exp_samples": [] if value is None else
                                      [{"labels": "{}", "value": value}]}
                                     for key, value in expected.items()]}

    tests = {"rule_files": [], "evaluation_interval": "15s", "fuzzy_compare": True, "tests": [
        fixture("known 99.88 percent availability and negative 20 percent remaining budget"),
        fixture("partial month shows provisional estimates and hides confirmed values", ready=0),
        fixture("no eligible traffic leaves SLI and budget undefined", traffic=False),
        fixture("failed scrape hides current estimates", up=0),
        fixture("stale scrape hides current estimates", stale=True),
        fixture("new histogram bucket cannot produce a false zero latency fraction", new_bucket=True),
    ]}
    with tempfile.TemporaryDirectory(prefix="sre-grafana-") as directory:
        location = Path(directory)
        # promtool runs as nobody in the container; these fixtures contain no secrets.
        location.chmod(0o755)
        (location / "rules.json").write_text(json.dumps({"groups": [{"name": "dashboards", "rules": expressions}]}))
        (location / "tests.json").write_text(json.dumps(tests))
        for file in location.iterdir():
            file.chmod(0o644)
        command = shlex.split(args.compose) + ["run", "--rm", "--no-deps", "--volume",
                                              directory + ":/dashboard-tests:ro", "--entrypoint", "promtool", "prometheus"]
        for action, file in [("check", "rules.json"), ("test", "tests.json")]:
            subprocess.run(command + [action, "rules", "/dashboard-tests/" + file], cwd=ROOT, check=True)
    print(f"Validated {len(uids)} dashboards, {len(expressions)} PromQL expressions and {len(tests['tests'])} SLO scenarios.")


if __name__ == "__main__":
    main()
