// RabbitMQ consumption (importer-worker). Items are created through the REST
// API, so each one goes the real way: outbox, relay, exchange "importer",
// queue importer-worker.item-created, worker. An item is processed when its
// status is "done"; processedAt - createdAt (both set by the server) is how
// long that took, with no error from how often k6 polls.
//
// MODE=latency (default): a steady RATE of creates per second for DURATION;
//   each iteration waits for its own item to be done. Answers "how long does
//   an item take to be processed at this rate?"
// MODE=backlog: create COUNT items as fast as possible, then wait for all of
//   them. Answers "how fast does the worker drain a burst?"
//
//   make perf-worker RATE=20 DURATION=3m
//   make perf-worker MODE=backlog COUNT=2000
import http from 'k6/http';
import { check, sleep, fail } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { API_URL, JSON_HEADERS, PREFIX, createItemREST, deleteItemREST, parseTime } from './lib/common.js';

const MODE = __ENV.MODE || 'latency';
const TIMEOUT_S = Number(__ENV.TIMEOUT || 60); // give up waiting for an item after this

const processing = new Trend('item_processing_ms', true); // createdAt -> processedAt
const unprocessed = new Counter('items_unprocessed'); // still pending at TIMEOUT
const drain = new Trend('backlog_drain_s'); // backlog: first create -> last processed
const afterCreates = new Trend('backlog_drain_after_creates_s'); // backlog: last create -> last processed
const throughput = new Trend('backlog_throughput_per_s'); // backlog: COUNT / drain

const modes = {
  latency: {
    scenarios: {
      latency: {
        executor: 'constant-arrival-rate',
        rate: Number(__ENV.RATE || 10), timeUnit: '1s', duration: __ENV.DURATION || '1m',
        // Each iteration waits for the worker, so it needs more VUs than the rate.
        preAllocatedVUs: 50, maxVUs: 500,
        exec: 'latency',
      },
    },
    thresholds: {
      'item_processing_ms': ['p(95)<2000'],
      'items_unprocessed': ['count==0'],
      http_req_failed: ['rate<0.01'],
    },
  },
  backlog: {
    setupTimeout: '15m',
    scenarios: {
      backlog: { executor: 'per-vu-iterations', vus: 1, iterations: 1, maxDuration: '30m', exec: 'backlog' },
    },
    thresholds: { 'items_unprocessed': ['count==0'] },
  },
};
if (!modes[MODE]) throw new Error(`unknown MODE "${MODE}": use latency or backlog`);
export const options = { ...modes[MODE], teardownTimeout: '10m' };

// --- latency ---

export function latency() {
  const item = createItemREST('import');
  if (!item) return;

  const deadline = Date.now() + TIMEOUT_S * 1000;
  let done = null;
  while (Date.now() < deadline) {
    const res = http.get(`${API_URL}/api/items/${item.id}`, { tags: { name: 'poll' } });
    if (res.status === 200 && res.json('status') === 'done') {
      done = res.json();
      break;
    }
    sleep(0.1);
  }

  if (done) {
    processing.add(parseTime(done.processedAt) - parseTime(done.createdAt));
  } else {
    unprocessed.add(1);
  }
  deleteItemREST(item.id);
}

// --- backlog ---

const COUNT = Number(__ENV.COUNT || 1000);
const BATCH = 50; // creates in flight at once

export function setup() {
  if (MODE !== 'backlog') return {};
  const ids = [];
  for (let i = 0; i < COUNT; i += BATCH) {
    const reqs = [];
    for (let j = i; j < Math.min(i + BATCH, COUNT); j++) {
      reqs.push(['POST', `${API_URL}/api/items`, JSON.stringify({ name: `${PREFIX}-${j}`, kind: 'import' }),
        { headers: JSON_HEADERS, tags: { name: 'POST /api/items' } }]);
    }
    for (const res of http.batch(reqs)) {
      if (check(res, { 'create: 201': (r) => r.status === 201 })) ids.push(res.json('id'));
    }
  }
  if (ids.length === 0) fail('no items were created');
  console.log(`created ${ids.length}/${COUNT} items, waiting for the worker`);
  return { ids };
}

export function backlog(data) {
  const want = new Set(data.ids);
  const deadline = Date.now() + TIMEOUT_S * 1000;
  let mine = [];
  // One list per second rather than a GET per item: the list is every item in
  // the table, but it's one request instead of thousands.
  while (Date.now() < deadline) {
    const res = http.get(`${API_URL}/api/items`, { tags: { name: 'GET /api/items' } });
    if (res.status === 200) {
      mine = res.json().filter((it) => want.has(it.id));
      const done = mine.filter((it) => it.status === 'done').length;
      console.log(`${done}/${want.size} done`);
      if (done === want.size) break;
    }
    sleep(1);
  }

  const done = mine.filter((it) => it.status === 'done');
  unprocessed.add(want.size - done.length);
  if (done.length === 0) return;

  const created = mine.map((it) => parseTime(it.createdAt));
  const processed = done.map((it) => parseTime(it.processedAt));
  for (const it of done) processing.add(parseTime(it.processedAt) - parseTime(it.createdAt));
  const first = Math.min(...created), lastCreate = Math.max(...created), last = Math.max(...processed);
  drain.add((last - first) / 1000);
  afterCreates.add((last - lastCreate) / 1000);
  throughput.add(done.length / ((last - first) / 1000));
}

export function teardown(data) {
  const ids = data.ids || [];
  for (let i = 0; i < ids.length; i += BATCH) {
    http.batch(ids.slice(i, i + BATCH).map((id) => ['DELETE', `${API_URL}/api/items/${id}`, null,
      { tags: { name: 'DELETE /api/items/{id}' } }]));
  }
}
