// Shared by the k6 scripts: targets, load profiles and item helpers.
import http from 'k6/http';
import { check } from 'k6';

// Defaults are the cluster's NodePorts. From the k6 container, `make run-api`
// on your machine is http://host.docker.internal:8080 (and :8081 for GraphQL).
export const API_URL = __ENV.API_URL || 'http://192.168.68.57:30080';
export const GRAPHQL_URL = __ENV.GRAPHQL_URL || 'http://192.168.68.57:30082/graphql';

// Every item a run creates is named after the run, so it's easy to find (and
// delete) if a run is interrupted before it cleans up.
export const RUN_ID = __ENV.RUN_ID || `${Date.now()}`;
export const PREFIX = `perf-${RUN_ID}`;

// PROFILE picks how hard to push. smoke checks that the script works; load is
// the expected traffic; stress keeps climbing to find where it breaks.
const profiles = {
  smoke: { executor: 'constant-arrival-rate', rate: 1, timeUnit: '1s', duration: '30s', preAllocatedVUs: 2 },
  load: {
    executor: 'ramping-arrival-rate',
    startRate: 5, timeUnit: '1s', preAllocatedVUs: 20, maxVUs: 100,
    stages: [
      { target: 20, duration: '1m' },
      { target: 20, duration: '3m' },
      { target: 0, duration: '30s' },
    ],
  },
  stress: {
    executor: 'ramping-arrival-rate',
    startRate: 10, timeUnit: '1s', preAllocatedVUs: 50, maxVUs: 300,
    stages: [
      { target: 50, duration: '2m' },
      { target: 100, duration: '2m' },
      { target: 200, duration: '2m' },
      { target: 0, duration: '30s' },
    ],
  },
};

// RATE replaces the profile with your own peak: ramp up to RATE iterations/s
// over RAMP, hold it for DURATION, then ramp down.
function custom(rate) {
  return {
    executor: 'ramping-arrival-rate',
    startRate: 1, timeUnit: '1s',
    // An iteration takes ~30ms at rest; the extra VUs are there for when the
    // services slow down, so k6 can keep the rate instead of falling behind.
    preAllocatedVUs: Math.max(10, rate), maxVUs: Math.max(50, rate * 10),
    stages: [
      { target: rate, duration: __ENV.RAMP || '1m' },
      { target: rate, duration: __ENV.DURATION || '3m' },
      { target: 0, duration: '30s' },
    ],
  };
}

export function scenario(name = __ENV.PROFILE || 'smoke') {
  if (__ENV.RATE) return custom(Number(__ENV.RATE));
  const p = profiles[name];
  if (!p) throw new Error(`unknown PROFILE "${name}": use ${Object.keys(profiles).join(', ')}`);
  return p;
}

export const JSON_HEADERS = { 'Content-Type': 'application/json' };

export function createItemREST(kind = 'import', tags = {}) {
  const res = http.post(
    `${API_URL}/api/items`,
    JSON.stringify({ name: `${PREFIX}-${__VU}-${__ITER}`, kind }),
    { headers: JSON_HEADERS, tags: { name: 'POST /api/items', ...tags } },
  );
  const ok = check(res, { 'create: 201': (r) => r.status === 201 });
  return ok ? res.json() : null;
}

export function deleteItemREST(id) {
  return http.del(`${API_URL}/api/items/${id}`, null, { tags: { name: 'DELETE /api/items/{id}' } });
}

// Go writes timestamps with nanoseconds; JavaScript's Date only parses up to
// milliseconds, so cut the fraction to 3 digits first.
export function parseTime(s) {
  return Date.parse(s.replace(/(\.\d{3})\d+/, '$1'));
}
