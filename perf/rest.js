// REST API (importer-api): one iteration is an item's life cycle, create, read
// and delete, plus a list on 1 iteration in 10.
//
//   make perf-rest PROFILE=load
import http from 'k6/http';
import { check } from 'k6';
import { API_URL, scenario, createItemREST, deleteItemREST } from './lib/common.js';

export const options = {
  scenarios: { rest: scenario() },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    'http_req_duration{name:POST /api/items}': ['p(95)<300'],
    'http_req_duration{name:GET /api/items/{id}}': ['p(95)<150'],
    'http_req_duration{name:DELETE /api/items/{id}}': ['p(95)<200'],
    // The list has no pagination, so it gets slower as the table grows.
    'http_req_duration{name:GET /api/items}': ['p(95)<1000'],
  },
};

export default function () {
  const item = createItemREST(__ITER % 2 === 0 ? 'import' : 'export');
  if (!item) return;

  const got = http.get(`${API_URL}/api/items/${item.id}`, { tags: { name: 'GET /api/items/{id}' } });
  check(got, {
    'get: 200': (r) => r.status === 200,
    'get: same item': (r) => r.json('id') === item.id,
  });

  if (__ITER % 10 === 0) {
    const list = http.get(`${API_URL}/api/items`, { tags: { name: 'GET /api/items' } });
    check(list, { 'list: 200': (r) => r.status === 200 });
  }

  check(deleteItemREST(item.id), { 'delete: 204': (r) => r.status === 204 });
}
