// Shell commands as run_js gets them: the packaged mcp-exec, started by
// exec-server.sh exactly as the entrypoint starts it, called over HTTP the
// way mcp-js calls an upstream MCP server. Run by the flake's `exec-smoke`
// check, which the image build depends on (Xvnc and openbox already up on
// $DISPLAY). Without DISPLAY the part that opens a window is skipped.
//
//   EXEC_SERVER=<path to exec-server.sh> node exec-smoke.mjs   (mcp-exec on PATH)
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

const PORT = 18082;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const home = fs.realpathSync(process.env.HOME || os.homedir());
const logDir = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-smoke-logs-'));

// What a previous pod leaves on the session's disk: a command that was
// running when the pod went away, and logs from long ago.
const STALE = '11111111-1111-4111-8111-111111111111';
const OLD = '22222222-2222-4222-8222-222222222222';
for (const id of [STALE, OLD]) {
  fs.writeFileSync(path.join(logDir, `${id}.log`), 'before the restart\n');
  fs.writeFileSync(path.join(logDir, `${id}.meta`), 'make all');
  fs.writeFileSync(path.join(logDir, `${id}.status`), id === STALE ? '"Running"' : '{"Completed":0}');
}
const longAgo = new Date(Date.now() - 30 * 86400 * 1000);
for (const ext of ['log', 'meta', 'status']) fs.utimesSync(path.join(logDir, `${OLD}.${ext}`), longAgo, longAgo);

const server = spawn('bash', [process.env.EXEC_SERVER], {
  env: { ...process.env, HOME: home, EXEC_LOG_DIR: logDir, EXEC_MCP_PORT: String(PORT) },
  stdio: 'inherit',
});

// node:http, not fetch: fetch adds Sec-Fetch-* headers of its own, which is
// exactly what the server is told to refuse.
function post(body, headers, host = '127.0.0.1') {
  return new Promise((resolve, reject) => {
    const req = http.request({ host, port: PORT, path: '/mcp', method: 'POST', headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }));
    });
    req.on('error', reject);
    req.end(body);
  });
}

const MCP_HEADERS = { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' };
const INITIALIZE = {
  jsonrpc: '2.0',
  id: 0,
  method: 'initialize',
  params: { protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'exec-smoke', version: '1' } },
};

// The JSON-RPC message of a response, which arrives as JSON or as one SSE
// event among keep-alives.
function message(res) {
  if (String(res.headers['content-type']).startsWith('application/json')) return JSON.parse(res.body);
  const data = res.body
    .split('\n')
    .filter((line) => line.startsWith('data: {'))
    .map((line) => JSON.parse(line.slice(6)));
  assert.ok(data.length, `no message in: ${res.body}`);
  return data[data.length - 1];
}

let session;
let nextId = 1;
async function rpc(method, params) {
  const res = await post(JSON.stringify({ jsonrpc: '2.0', id: nextId++, method, params }), { ...MCP_HEADERS, 'Mcp-Session-Id': session });
  assert.equal(res.status, 200, res.body);
  const msg = message(res);
  assert.ok(!msg.error, JSON.stringify(msg.error));
  return msg.result;
}

// A tool call; mcp-exec answers with JSON in a text item.
async function call(name, args) {
  const out = await rpc('tools/call', { name, arguments: args });
  assert.ok(!out.isError, JSON.stringify(out));
  return JSON.parse(out.content[0].text);
}

// exec, then stream_logs from the offset it returns until the command ended:
// the flow run_js.md documents.
async function run(label, cmd, timeout = 20) {
  const started = Date.now();
  const { id, status } = await call('exec', { cmd, timeout });
  assert.equal(status, 'started');
  let logs = '';
  let offset = 0;
  for (;;) {
    const r = await call('stream_logs', { id, offset });
    logs += r.logs;
    offset = r.next_offset;
    if (r.status !== 'running') {
      console.log(`ok   ${label}: ${r.status} (${Date.now() - started} ms)`);
      return { id, logs, status: r.status, offset };
    }
    assert.ok(Date.now() - started < 60000, `${label}: still running: ${logs}`);
    await sleep(100);
  }
}

try {
  for (let i = 0; ; i++) {
    try {
      await post('', {});
      break;
    } catch (err) {
      assert.equal(server.exitCode, null, 'exec-server.sh exited');
      if (i > 100) throw err;
      await sleep(100);
    }
  }

  const init = await post(JSON.stringify(INITIALIZE), MCP_HEADERS);
  assert.equal(init.status, 200, init.body);
  session = init.headers['mcp-session-id'];
  assert.ok(session, 'no session id');
  assert.equal((await post(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }), { ...MCP_HEADERS, 'Mcp-Session-Id': session })).status, 202);

  // The tools and their arguments are what docs/contracts/policy/tools/ says.
  const { tools } = await rpc('tools/list', {});
  const shape = Object.fromEntries(
    tools.map((t) => [t.name, Object.fromEntries(Object.entries(t.inputSchema.properties).map(([k, v]) => [k, v.type]))]),
  );
  assert.deepEqual(shape, {
    exec: { cmd: 'string', timeout: 'integer' },
    search_logs: { id: 'string', pattern: 'string' },
    stream_logs: { id: 'string', offset: 'integer' },
  });
  for (const t of tools) assert.deepEqual([...t.inputSchema.required].sort(), Object.keys(t.inputSchema.properties).sort());
  console.log('ok   tools: exec {cmd, timeout}, stream_logs {id, offset}, search_logs {id, pattern}');

  // A command end to end: who and where it is, both streams, the exit code.
  let r = await run('a command, its output and exit code', 'printf "%s|%s|%s\\n" "$(id -u)" "$PWD" "$DISPLAY"; echo problem >&2; echo "a b" | tr a-z A-Z; exit 3');
  assert.equal(r.status, 'completed:3');
  assert.deepEqual(r.logs.split('\n').sort(), ['', `${process.getuid()}|${home}|${process.env.DISPLAY || ''}`, 'A B', 'problem'].sort());
  assert.equal(r.offset, Buffer.byteLength(r.logs));
  assert.deepEqual(await call('stream_logs', { id: r.id, offset: r.offset }), { logs: '', next_offset: r.offset, status: 'completed:3' });
  assert.deepEqual((await call('search_logs', { id: r.id, pattern: '^prob' })).matches.map((m) => m.line), ['problem']);
  console.log('ok   stream_logs from an offset, search_logs');

  // Output arrives while the command runs.
  const slow = await call('exec', { cmd: 'echo first; sleep 2; echo second', timeout: 20 });
  let seen;
  for (let i = 0; i < 50; i++) {
    seen = await call('stream_logs', { id: slow.id, offset: 0 });
    if (seen.logs) break;
    await sleep(100);
  }
  assert.deepEqual([seen.logs, seen.status], ['first\n', 'running']);
  console.log('ok   output is readable while the command runs');

  r = await run('a command past its timeout', 'exec sleep 30', 1);
  assert.equal(r.status, 'timeout');

  // The logs are files in EXEC_LOG_DIR, and the previous pod's are still
  // readable: its running command is reported as interrupted, old logs are gone.
  assert.ok(fs.existsSync(path.join(logDir, `${r.id}.log`)));
  const stale = await call('stream_logs', { id: STALE, offset: 0 });
  assert.equal(stale.logs, 'before the restart\n');
  assert.match(stale.status, /^failed:interrupted/);
  assert.equal(fs.existsSync(path.join(logDir, `${OLD}.log`)), false);
  console.log(`ok   a command the previous pod left running: ${stale.status}`);

  // Only mcp-js is served. What a page in the session's browser sends:
  const body = JSON.stringify(INITIALIZE);
  const refused = async (label, headers, status, host) => {
    const res = await post(body, { ...MCP_HEADERS, ...headers }, host);
    assert.equal(res.status, status, `${label}: ${res.status} ${res.body}`);
    console.log(`ok   refused (${status}): ${label}`);
  };
  await refused('a cross-origin request (Origin)', { Origin: 'https://example.com' }, 403);
  await refused('a request from a page on loopback (Origin)', { Origin: `http://127.0.0.1:${PORT}` }, 403);
  await refused('an opaque origin', { Origin: 'null' }, 403);
  await refused('fetch metadata without Origin (Sec-Fetch-*)', { 'Sec-Fetch-Site': 'same-origin', 'Sec-Fetch-Mode': 'cors' }, 403);
  await refused('a no-cors form post', { 'Content-Type': 'text/plain', 'Sec-Fetch-Mode': 'no-cors' }, 403);
  await refused('a plain-text body without browser headers', { 'Content-Type': 'text/plain' }, 415);
  await refused('another Host (DNS rebinding)', { Host: `attacker.example:${PORT}` }, 403);
  // And it listens on loopback only: nothing on the machine's other addresses.
  const others = Object.values(os.networkInterfaces()).flat().filter((a) => !a.internal && a.family === 'IPv4');
  for (const { address } of others) {
    const open = await new Promise((resolve) => {
      const s = net.connect({ host: address, port: PORT, timeout: 2000 });
      s.once('connect', () => resolve(true)).once('error', () => resolve(false)).once('timeout', () => resolve(false));
    });
    assert.equal(open, false, `port ${PORT} is open on ${address}`);
    console.log(`ok   not listening on ${address}`);
  }
  if (!others.length) console.log('skip no address other than loopback to check');

  if (process.env.DISPLAY) {
    // A command can put a window on the desktop the person watches.
    const title = 'exec-smoke-window';
    const term = await call('exec', { cmd: `xterm -fa "DejaVu Sans Mono" -T ${title} -e sleep 4`, timeout: 20 });
    let tree = '';
    for (let i = 0; i < 30 && !tree.includes(title); i++) {
      await sleep(200);
      tree = (await run('the windows of the display', 'xwininfo -root -tree')).logs;
    }
    assert.ok(tree.includes(title), `no window titled ${title}:\n${tree}`);
    for (let i = 0; i < 100; i++) {
      if ((await call('stream_logs', { id: term.id, offset: 0 })).status !== 'running') break;
      await sleep(100);
    }
    assert.equal((await call('stream_logs', { id: term.id, offset: 0 })).status, 'completed:0');
    console.log('ok   a window appeared on the display');
  } else {
    console.log('skip the window on the display: no DISPLAY');
  }

  console.log('exec smoke test passed');
} finally {
  server.kill();
  fs.rmSync(logDir, { recursive: true, force: true });
}
process.exit(0);
