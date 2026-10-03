// Usage: node test/package-smoke.mjs /absolute/path/to/computer-use-mcp
// Exercises the actual packaged gateway and both stdio upstreams, without CDP or a display.
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Client } from '../browser/node_modules/@modelcontextprotocol/sdk/dist/esm/client/index.js';
import { StdioClientTransport } from '../browser/node_modules/@modelcontextprotocol/sdk/dist/esm/client/stdio.js';

const command = process.argv[2];
assert.ok(command, 'pass the built executable path');
const state = await mkdtemp(join(tmpdir(), 'computer-use-package-test-'));
const transport = new StdioClientTransport({ command,
  env: { ...process.env, COMPUTER_USE_STATE_DIR: state }, stderr: 'pipe' });
let stderr = '';
transport.stderr?.on('data', chunk => stderr += chunk);
const client = new Client({ name: 'package-smoke', version: '1' });
try {
  await client.connect(transport);
  const { tools } = await client.listTools();
  for (const suffix of ['browser__browser_execute', 'browser__desktop_execute', 'exec__exec']) {
    assert.ok(tools.some(tool => tool.name.endsWith(suffix)), `missing ${suffix}`);
  }
  const result = await client.callTool({ name: 'run_js', arguments: { code: `
    const response = await mcp.callTool('exec', 'exec', {
      bin: ${JSON.stringify(process.execPath)}, args: ['-e', 'console.log("package-exec-ok")'], timeout: 10
    });
    const started = JSON.parse(response.content[0].text);
    if (!started.id) throw new Error(JSON.stringify(started));
    let offset = 0;
    for (let attempt = 0; attempt < 100; attempt++) {
      const response = await mcp.callTool('exec', 'stream_logs', {id: started.id, offset});
      const log = JSON.parse(response.content[0].text);
      console.log(log.logs);
      offset = log.next_offset;
      if (log.status !== 'running') break;
      await new Promise(resolve => setTimeout(resolve, 50));
    }
  ` } });
  assert.ok(!result.isError, JSON.stringify(result));
  assert.ok(JSON.stringify(result).includes('package-exec-ok'), JSON.stringify(result));
  console.log('Packaged gateway, browser/desktop discovery, and exec over stdio passed');
} catch (error) {
  console.error(stderr);
  throw error;
} finally {
  await client.close();
  await rm(state, { recursive: true, force: true });
}
