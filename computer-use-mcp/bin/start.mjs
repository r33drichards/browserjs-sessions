#!/usr/bin/env node
// Local stdio gateway. Hosted containers use their existing entrypoints.
import { spawn } from 'node:child_process';
import { mkdtemp, writeFile, rm, mkdir } from 'node:fs/promises';
import { tmpdir, homedir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';

const root = fileURLToPath(new URL('../', import.meta.url));
const children = new Set();
let directory;
let stopping = false;
async function stop(code) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill('SIGTERM');
  const force = setTimeout(() => {
    for (const child of children) child.kill('SIGKILL');
  }, 2000);
  await Promise.all([...children].map(child => new Promise(resolve => child.once('close', resolve))));
  clearTimeout(force);
  if (directory) await rm(directory, { recursive: true, force: true });
  process.exit(code);
}
function launch(command, args, options = {}) {
  const child = spawn(command, args, { stdio: ['ignore', 'inherit', 'inherit'], ...options });
  children.add(child);
  child.once('error', error => {
    console.error(`Could not start ${command}: ${error.message}`);
    void stop(1);
  });
  child.once('close', code => {
    children.delete(child);
    if (!stopping) void stop(code ?? 1);
  });
  return child;
}
function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const port = server.address().port;
      server.close(error => error ? reject(error) : resolve(port));
    });
  });
}
async function ready(port) {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (stopping) throw new Error('server stopped during startup');
    const connected = await new Promise(resolve => {
      const socket = net.connect(port, '127.0.0.1');
      socket.setTimeout(200);
      socket.once('connect', () => { socket.destroy(); resolve(true); });
      socket.once('error', () => resolve(false));
      socket.once('timeout', () => { socket.destroy(); resolve(false); });
    });
    if (connected) return;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error('browser MCP did not start within 10 seconds');
}
process.once('SIGTERM', () => void stop(0));
process.once('SIGINT', () => void stop(0));
try {
  directory = await mkdtemp(join(tmpdir(), 'computer-use-mcp-'));
  const state = resolve(process.env.COMPUTER_USE_STATE_DIR || join(homedir(), '.computer-use-mcp'));
  await mkdir(state, { recursive: true, mode: 0o700 });
  const port = await freePort();
  launch(process.execPath, [join(root, 'browser/server.js')], {
    // Keep stdout exclusively for the gateway's MCP protocol.
    stdio: ['ignore', 'ignore', 'inherit'],
    env: { ...process.env, BROWSER_MCP_PORT: String(port), BROWSER_MCP_HOST: '127.0.0.1',
      TAB_STATE_FILE: join(state, 'tabs.json') },
  });
  await ready(port);
  const servers = [{ name: 'browser', transport: 'http', url: `http://127.0.0.1:${port}/mcp` }];
  if (process.env.COMPUTER_USE_EXEC_URL) {
    servers.push({ name: 'exec', transport: 'http', url: process.env.COMPUTER_USE_EXEC_URL });
  }
  const config = join(directory, 'servers.json');
  await writeFile(config, JSON.stringify(servers));
  const gatewayEnv = { ...process.env };
  delete gatewayEnv.MCP_V8_HTTP_PORT;
  delete gatewayEnv.MCP_V8_CONFIG;
  const policies = JSON.stringify({ mcp_tools: { policies: [
    { url: new URL('../code-mode/mcp_tools.rego', import.meta.url).href },
  ] } });
  launch(process.env.MCP_V8_BIN || 'mcp-v8', [
    '--stateless', '--mcp-config', config, '--policies-json', policies,
    '--session-db-path', join(state, 'sessions'),
  ], {
    stdio: 'inherit',
    env: { ...gatewayEnv, MCP_V8_HEAP_STORE: 'none',
      MCP_V8_MCP_CONFIG: config,
      MCP_V8_SESSION_DB_PATH: join(state, 'sessions'),
      MCP_V8_POLICIES_JSON: policies,
      MCP_V8_INSTRUCTIONS: '@' + join(root, 'local-instructions.md'),
      MCP_V8_RUN_JS_DESCRIPTION: 'Runs JavaScript in a fresh isolate. Use mcp.callTool("browser", "browser_execute", {operations: [...]}) or mcp.callTool("browser", "desktop_execute", {operations: [...]}). Call console.log to return text. Host filesystem and network access are disabled.',
    },
  });
} catch (error) {
  console.error(error.message);
  await stop(1);
}
