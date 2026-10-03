import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

test('local launcher serves MCP, reserves stdout, and cleans up after gateway exit', { timeout: 20000 }, async t => {
  const dir = await mkdtemp(join(tmpdir(), 'cu-launcher-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const gateway = join(dir, 'gateway.mjs');
  await writeFile(gateway, `#!${process.execPath}
import { readFile, writeFile } from 'node:fs/promises';
import http from 'node:http';
const path = process.env.MCP_V8_MCP_CONFIG;
const [server] = JSON.parse(await readFile(path, 'utf8'));
const response = await new Promise((resolve, reject) => {
 const request = http.request(server.url, {method: 'POST', headers: {
 'content-type': 'application/json', accept: 'application/json, text/event-stream'
 }}, response => {
 let text = '';
 response.on('data', chunk => text += chunk);
 response.on('end', () => resolve({status: response.statusCode, text}));
 });
 request.on('error', reject);
 request.end(JSON.stringify({jsonrpc: '2.0', id: 1, method: 'tools/list', params: {}}));
});
if (response.status !== 200 || !response.text.includes('browser_execute') || !response.text.includes('desktop_execute')) process.exit(2);
await writeFile(process.env.TEST_CONFIG_PATH, path);
console.log('gateway-stdio');
`, { mode: 0o700 });
  const record = join(dir, 'config-path');
  const child = spawn(process.execPath, [fileURLToPath(new URL('../bin/start.mjs', import.meta.url))], {
    env: { ...process.env, MCP_V8_BIN: gateway, COMPUTER_USE_STATE_DIR: join(dir, 'state'), TEST_CONFIG_PATH: record },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  t.after(() => child.kill('SIGKILL'));
  let stdout = '', stderr = '';
  child.stdout.on('data', chunk => stdout += chunk);
  child.stderr.on('data', chunk => stderr += chunk);
  const code = await new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('close', resolve);
  });
  assert.equal(code, 0, stderr);
  assert.equal(stdout, 'gateway-stdio\n');
  const { readFile, access } = await import('node:fs/promises');
  const config = await readFile(record, 'utf8');
  await assert.rejects(access(config), { code: 'ENOENT' });
  assert.deepEqual(await readdir(join(dir, 'state')), []);
});
