#!/usr/bin/env node
// Local stdio gateway. Hosted containers use their existing entrypoints.
import { spawn } from 'node:child_process';
import { mkdtemp, writeFile, rm, mkdir } from 'node:fs/promises';
import { tmpdir, homedir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

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
process.once('SIGTERM', () => void stop(0));
process.once('SIGINT', () => void stop(0));
try {
  directory = await mkdtemp(join(tmpdir(), 'computer-use-mcp-'));
  const state = resolve(process.env.COMPUTER_USE_STATE_DIR || join(homedir(), '.computer-use-mcp'));
  await mkdir(state, { recursive: true, mode: 0o700 });
  const servers = [{ name: 'browser', transport: 'stdio',
    command: process.execPath,
    args: [process.env.COMPUTER_USE_BROWSER_SERVER || join(root, 'browser/server.js'), '--stdio'],
    env: { TAB_STATE_FILE: join(state, 'tabs.json') },
  }];
  if (process.env.COMPUTER_USE_EXEC_URL) {
    servers.push({ name: 'exec', transport: 'http', url: process.env.COMPUTER_USE_EXEC_URL });
  } else if (process.env.MCP_EXEC_BIN) {
    servers.push({ name: 'exec', transport: 'stdio', command: process.env.MCP_EXEC_BIN,
      args: ['--directory-path', join(state, 'exec-logs')] });
  }
  const config = join(directory, 'servers.json');
  await writeFile(config, JSON.stringify(servers));
  const gatewayEnv = { ...process.env };
  delete gatewayEnv.MCP_V8_HTTP_PORT;
  delete gatewayEnv.MCP_V8_SSE_PORT;
  delete gatewayEnv.MCP_V8_CONFIG;
  const policies = JSON.stringify({ mcp_tools: { policies: [
    { url: new URL('../code-mode/mcp_tools.rego', import.meta.url).href },
  ] } });
  launch(process.env.MCP_V8_BIN || 'mcp-v8', [
    '--heap-store', 'none', '--mcp-config', config, '--policies-json', policies,
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
