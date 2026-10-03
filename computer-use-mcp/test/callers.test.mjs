// Usage: node --test computer-use-mcp/test/callers.test.mjs  (needs only node)
import assert from 'node:assert/strict';
import http from 'node:http';
import { after, before, test } from 'node:test';
import { mcpCallerRefusal } from '../browser/callers.js';

let server, port;
before(async () => {
  // The guard as server.js applies it, in front of a stand-in for the MCP handler.
  server = http.createServer((req, res) => {
    const refused = mcpCallerRefusal(req);
    if (refused) return res.writeHead(403).end('forbidden');
    res.writeHead(200).end('handled');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  port = server.address().port;
});
after(() => server.close());

function post(headers) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method: 'POST', path: '/mcp', headers }, (res) => {
      res.resume();
      res.on('end', () => resolve(res.statusCode));
    });
    req.on('error', reject);
    req.end('{}');
  });
}

test('mcp-js, over loopback, is served', async () => {
  assert.equal(await post({ 'content-type': 'application/json' }), 200);
  assert.equal(await post({ 'content-type': 'application/json; charset=utf-8', host: 'localhost:8081' }), 200);
});

test('a request made by a page in the browser is refused', async () => {
  // What Chromium attaches to a page's request; page script cannot drop these.
  assert.equal(await post({ 'content-type': 'text/plain', origin: 'https://example.com', 'sec-fetch-site': 'cross-site', 'sec-fetch-mode': 'no-cors' }), 403);
  assert.equal(await post({ 'content-type': 'application/json', origin: 'null' }), 403);
  assert.equal(await post({ 'content-type': 'application/json', 'sec-fetch-site': 'same-origin' }), 403);
  // A form post or a beacon: no JSON content type.
  assert.equal(await post({ 'content-type': 'text/plain' }), 403);
  assert.equal(await post({ 'content-type': 'application/x-www-form-urlencoded' }), 403);
});

test('a name pointed at loopback is refused', async () => {
  assert.equal(await post({ 'content-type': 'application/json', host: 'rebound.example.com:8081' }), 403);
});
