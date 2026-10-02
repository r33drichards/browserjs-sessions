// The shell_execute and shell_process tools as mcp-js reaches them: the
// packaged server, over HTTP on /mcp. Run by the flake's `shell-smoke` check,
// which the image build depends on (Xvnc and openbox already up on $DISPLAY).
// Without DISPLAY the part that opens a window is skipped.
//
//   BROWSER_MCP=<path to the installed browser-mcp> node shell-smoke.mjs
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';

const PORT = 18081;
const URL = `http://127.0.0.1:${PORT}/mcp`;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const home = fs.realpathSync(process.env.HOME || os.homedir());
const server = spawn(process.env.BROWSER_MCP, [], {
  env: { ...process.env, BROWSER_MCP_PORT: String(PORT), HOME: home, FILES_DIR: '', TAB_STATE_FILE: '' },
  stdio: 'inherit',
});

// node:http, not fetch: fetch adds Sec-Fetch-* headers, which mark a request
// as coming from a web page, and the server refuses those (callers.js).
function post(body, headers) {
  return new Promise((resolve, reject) => {
    const req = http.request(URL, { method: 'POST', headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString('utf8') }));
    });
    req.on('error', reject);
    req.end(body);
  });
}

let nextId = 1;
async function rpc(method, params, headers = {}) {
  const res = await post(JSON.stringify({ jsonrpc: '2.0', id: nextId++, method, params }), {
    'Content-Type': 'application/json',
    Accept: 'application/json, text/event-stream',
    ...headers,
  });
  if (res.status !== 200) return { status: res.status };
  const body = JSON.parse(res.body);
  assert.ok(!body.error, JSON.stringify(body.error));
  return body.result;
}

// A tool call that must not be an error; its JSON result.
async function call(label, name, args) {
  const started = Date.now();
  const out = await rpc('tools/call', { name, arguments: args });
  assert.ok(!out.isError, `${label}: ${out.content?.[0]?.text}`);
  console.log(`ok   ${label} (${Date.now() - started} ms)`);
  return JSON.parse(out.content[0].text);
}

try {
  for (let i = 0; ; i++) {
    try {
      await post('', {});
      break;
    } catch (err) {
      if (i > 100) throw err;
      await sleep(100);
    }
  }

  const { tools } = await rpc('tools/list', {});
  assert.deepEqual(tools.map((t) => t.name), ['browser_execute', 'desktop_execute', 'shell_execute', 'shell_process']);
  console.log('ok   the server lists shell_execute and shell_process');

  // Only mcp-js is served: a page in the session's browser is not.
  assert.equal((await rpc('tools/list', {}, { Origin: 'https://example.com' })).status, 403);
  console.log('ok   a request from a web page is refused');

  let r = await call('argv: who and where the command is', 'shell_execute', {
    argv: ['sh', '-c', 'printf "%s|%s|%s" "$(id -u)" "$PWD" "$HOME"; command -v bash >/dev/null'],
  });
  assert.deepEqual([r.exit_code, r.stdout], [0, `${process.getuid()}|${home}|${home}`]);

  r = await call('script: bash, a pipe, a file in the home directory', 'shell_execute', {
    script: 'echo "smoke $((6 * 7))" | tee smoke.txt | tr a-z A-Z; exit 5',
    env: { SMOKE: '1' },
  });
  assert.deepEqual([r.exit_code, r.stdout], [5, 'SMOKE 42\n']);
  assert.equal(fs.readFileSync(path.join(home, 'smoke.txt'), 'utf8'), 'smoke 42\n');

  r = await call('timeout: the process group is ended', 'shell_execute', { script: 'sleep 60 & wait', timeout_ms: 500 });
  assert.deepEqual([r.timed_out, r.signal], [true, 'SIGTERM']);

  const refusedCall = await rpc('tools/call', { name: 'shell_execute', arguments: { argv: ['env'], env: { LD_PRELOAD: '/tmp/x.so' } } });
  assert.equal(refusedCall.isError, true);
  assert.match(refusedCall.content[0].text, /LD_PRELOAD may not be set/);
  console.log('ok   LD_PRELOAD is refused');

  const bg = await call('background: a command that keeps running', 'shell_execute', {
    script: 'i=0; while true; do i=$((i + 1)); echo "tick $i"; sleep 0.2; done',
    background: true,
  });
  await sleep(1500);
  r = await call('poll: its output so far', 'shell_process', { action: 'poll', id: bg.id });
  assert.equal(r.running, true);
  assert.match(r.stdout, /^tick 1\ntick 2\n/);
  r = await call('kill: it ends', 'shell_process', { action: 'kill', id: bg.id });
  assert.deepEqual([r.running, r.signal], [false, 'SIGTERM']);

  if (process.env.DISPLAY) {
    // A command can put a window on the desktop the person watches.
    const title = 'shell-smoke-window';
    const term = await call('background: a terminal window on the display', 'shell_execute', {
      argv: ['xterm', '-fa', 'DejaVu Sans Mono', '-T', title, '-e', 'sleep', '60'],
      background: true,
    });
    let tree = '';
    for (let i = 0; i < 50 && !tree.includes(title); i++) {
      await sleep(200);
      tree = (await call('the windows of the display', 'shell_execute', { argv: ['xwininfo', '-root', '-tree'] })).stdout;
    }
    assert.ok(tree.includes(title), `no window titled ${title}:\n${tree}`);
    r = await call('kill: the window closes', 'shell_process', { action: 'kill', id: term.id });
    assert.equal(r.running, false);
    await sleep(500);
    tree = (await call('the windows of the display', 'shell_execute', { argv: ['xwininfo', '-root', '-tree'] })).stdout;
    assert.ok(!tree.includes(title), 'the window is still there');
    console.log('     the window appeared on the display and went away when killed');
  } else {
    console.log('skip the window on the display: no DISPLAY');
  }

  const listed = await call('list: nothing left', 'shell_process', { action: 'list' });
  assert.deepEqual(listed.processes, []);
  console.log('shell smoke test passed');
} finally {
  server.kill();
}
process.exit(0);
