// GraphQL (importer-graphql): the same life cycle as rest.js, so the two can
// be compared run for run. Every request is POST /graphql, so requests are
// tagged with the operation name to tell them apart (the Importer GraphQL
// dashboard groups by the same names).
//
//   make perf-graphql PROFILE=load
import http from 'k6/http';
import { check } from 'k6';
import { GRAPHQL_URL, JSON_HEADERS, PREFIX, scenario } from './lib/common.js';

export const options = {
  scenarios: { graphql: scenario() },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    'http_req_duration{name:CreateItem}': ['p(95)<300'],
    'http_req_duration{name:Item}': ['p(95)<150'],
    'http_req_duration{name:DeleteItem}': ['p(95)<200'],
    'http_req_duration{name:Items}': ['p(95)<1000'],
  },
};

// GraphQL errors come back with HTTP 200, so a status check isn't enough:
// gqlCheck also fails when the body has an "errors" list.
function gql(operationName, query, variables) {
  const res = http.post(GRAPHQL_URL, JSON.stringify({ operationName, query, variables }), {
    headers: JSON_HEADERS,
    tags: { name: operationName },
  });
  check(res, {
    [`${operationName}: 200`]: (r) => r.status === 200,
    [`${operationName}: no errors`]: (r) => r.status === 200 && !r.json('errors'),
  });
  return res.status === 200 ? res.json('data') : null;
}

const CREATE = `mutation CreateItem($input: CreateItemInput!) {
  createItem(input: $input) { id name kind status }
}`;
const ITEM = `query Item($id: ID!) { item(id: $id) { id name kind status processedAt } }`;
const ITEMS = `query Items { items { id name status } }`;
const DELETE = `mutation DeleteItem($id: ID!) { deleteItem(id: $id) }`;

export default function () {
  const created = gql('CreateItem', CREATE, {
    input: { name: `${PREFIX}-${__VU}-${__ITER}`, kind: __ITER % 2 === 0 ? 'IMPORT' : 'EXPORT' },
  });
  const id = created && created.createItem && created.createItem.id;
  if (!id) return;

  const got = gql('Item', ITEM, { id });
  check(got, { 'Item: same item': (d) => d && d.item && d.item.id === id });

  if (__ITER % 10 === 0) gql('Items', ITEMS);

  const deleted = gql('DeleteItem', DELETE, { id });
  check(deleted, { 'DeleteItem: true': (d) => d && d.deleteItem === true });
}
