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
import { Client } from '${new URL('../browser/node_modules/@modelcontextprotocol/sdk/dist/esm/client/index.js', import.meta.url).href}';
import { StdioClientTransport } from '${new URL('../browser/node_modules/@modelcontextprotocol/sdk/dist/esm/client/stdio.js', import.meta.url).href}';
const path = process.env.MCP_V8_MCP_CONFIG;
const [server] = JSON.parse(await readFile(path, 'utf8'));
if (server.transport !== 'stdio' || server.args.at(-1) !== '--stdio') process.exit(2);
const client = new Client({name: 'launcher-test', version: '1'});
await client.connect(new StdioClientTransport({command: server.command, args: server.args,
 env: {...process.env, ...server.env}}));
const response = await client.listTools();
if (!response.tools.some(t => t.name === 'browser_execute') || !response.tools.some(t => t.name === 'desktop_execute')) process.exit(2);
const result = await client.callTool({name: 'browser_execute', arguments: {operations: [{type: 'invalid'}]}});
if (!result.isError) process.exit(3);
await client.close();
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
