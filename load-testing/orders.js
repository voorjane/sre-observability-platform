import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

function integer(name, fallback, min, max) {
  const value = Number(__ENV[name] ?? fallback);
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`${name} must be an integer between ${min} and ${max}`);
  }
  return value;
}

const baseURL = (__ENV.BASE_URL || 'http://127.0.0.1:8000').replace(/\/$/, '');
const rps = integer('LOAD_RPS', 30, 1, 1500);
const seconds = integer('LOAD_SECONDS', 60, 1, 300);
const warmup = __ENV.LOAD_PHASE === 'warmup';
const failures = new Rate('order_failure');
const transportFailures = new Counter('order_transport_failures');
const latency = new Trend('order_duration', true);
const success = http.expectedStatuses(201);
const payload = JSON.stringify({ item_id: 'load-item', customer_id: 'load-customer', amount: 49.99 });

export const options = {
  scenarios: {
    orders: {
      executor: 'constant-arrival-rate',
      rate: rps,
      timeUnit: '1s',
      duration: `${seconds}s`,
      preAllocatedVUs: Math.max(5, Math.ceil(rps * 0.12)),
      maxVUs: Math.max(10, Math.ceil(rps * 0.5)),
      gracefulStop: '6s',
    },
  },
  thresholds: warmup ? {} : {
    order_duration: ['p(95)<250'],
    order_failure: ['rate<0.001'],
    dropped_iterations: ['count==0'],
  },
  summaryTrendStats: ['min', 'med', 'avg', 'p(95)', 'max', 'count'],
};

export default function () {
  const res = http.post(`${baseURL}/api/v1/orders`, payload, {
    headers: { 'Content-Type': 'application/json' },
    timeout: '5s',
    redirects: 0,
    responseCallback: success,
    tags: { name: 'POST /api/v1/orders' },
  });
  let valid = false;
  if (res.status === 201) {
    try {
      const body = res.json();
      valid = typeof body?.order_id === 'string' && body.order_id.length > 0 &&
        body.status === 'COMPLETED' && body.payment_status === 'PROCESSED' &&
        typeof body.created_at === 'string' && Number.isFinite(Date.parse(body.created_at));
    } catch (_) {
      // A malformed success body counts as an unsuccessful order.
    }
  }
  check(res, { 'status is 201': (r) => r.status === 201, 'order completed': () => valid });
  failures.add(!valid);
  transportFailures.add(res.status === 0 ? 1 : 0);
  // Include request blocking and connection setup in the client duration.
  latency.add(res.timings.blocked + res.timings.duration);
}

export function handleSummary(data) {
  return { stdout: `K6_ORDER_SUMMARY=${JSON.stringify(data)}\n` };
}
