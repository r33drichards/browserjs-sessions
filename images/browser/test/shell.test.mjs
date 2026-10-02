// node --test images/browser/test/shell.test.mjs
//
// The shell_execute and shell_process tools (browser/shell.js), with real
// small commands: sh, bash and this node. SHELL_JS points the test at another
// copy of shell.js (the flake's shell-smoke runs it on the packaged one).
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test, { after } from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

const {
  DEFAULT_OUTPUT_BYTES,
  MAX_ARGS,
  MAX_BACKGROUND_TIMEOUT_MS,
  MAX_OUTPUT_BYTES,
  MAX_TIMEOUT_MS,
  SHELL_PROCESS_TOOL,
  SHELL_TOOL,
  createShell,
  cwdRefusal,
  defaultRoots,
  encodeOutput,
  envNameRefused,
  incompleteTail,
  validateExecute,
  validateProcess,
} = await import(process.env.SHELL_JS ? pathToFileURL(process.env.SHELL_JS).href : '../browser/shell.js');

const node = process.execPath;
// Real paths: on macOS the temporary directory is behind a symbolic link.
const home = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'shell-test-home-')));
const elsewhere = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'shell-test-out-')));
fs.mkdirSync(path.join(home, 'work'));
fs.symlinkSync(elsewhere, path.join(home, 'link-out'));
fs.symlinkSync(path.join(home, 'work'), path.join(home, 'link-in'));
fs.writeFileSync(path.join(home, 'file'), 'x');
const roots = [home];

// `bash -l` reads the profiles: keep this machine's out of the output (a
// terminal's session messages on macOS, whatever ~/.profile prints).
const env = { ...process.env, HOME: home };
for (const name of Object.keys(env)) if (name.startsWith('TERM_')) delete env[name];

const shell = createShell({ roots, home, env, killGraceMs: 300 });
after(() => {
  shell.stop();
  fs.rmSync(home, { recursive: true, force: true });
  fs.rmSync(elsewhere, { recursive: true, force: true });
});

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const alive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};
async function goneSoon(pid) {
  for (let i = 0; i < 50 && alive(pid); i++) await sleep(100);
  return !alive(pid);
}

// A call that must produce a result (whatever the exit code).
async function run(args) {
  const out = await shell.execute(args);
  assert.ok(!out.isError, out.content[0].text);
  return JSON.parse(out.content[0].text);
}
async function proc(args) {
  const out = await shell.process(args);
  assert.ok(!out.isError, out.content[0].text);
  return JSON.parse(out.content[0].text);
}
async function refused(args, pattern, tool = 'execute') {
  const out = await shell[tool](args);
  assert.equal(out.isError, true, JSON.stringify(out));
  assert.match(out.content[0].text, pattern);
}

test('argv runs the program directly: no shell reads the arguments', async () => {
  const args = ['$HOME', '*', 'a b', '; echo no', '`id`', '', "quo'te"];
  const r = await run({ argv: [node, '-e', 'console.log(JSON.stringify(process.argv.slice(1)))', ...args] });
  assert.deepEqual(JSON.parse(r.stdout), args);
  assert.deepEqual(
    { exit_code: r.exit_code, signal: r.signal, stderr: r.stderr, truncated: r.truncated, timed_out: r.timed_out },
    { exit_code: 0, signal: null, stderr: '', truncated: false, timed_out: false },
  );
  assert.equal(typeof r.duration_ms, 'number');
  assert.deepEqual(Object.keys(r), ['exit_code', 'signal', 'stdout', 'stderr', 'truncated', 'duration_ms', 'timed_out']);
});

test('script runs under bash, and a non-zero exit is a result', async () => {
  const r = await run({ script: 'echo out; echo err >&2; echo "$0 $BASH_VERSION" | cut -c1-5; exit 3' });
  assert.equal(r.stdout, 'out\nbash \n');
  assert.equal(r.stderr, 'err\n');
  assert.equal(r.exit_code, 3);
});

test('the environment is the server\'s, plus env; the home directory is the default cwd', async () => {
  const withEnv = createShell({ roots, home, env: { ...env, DISPLAY: ':99' } });
  const out = await withEnv.execute({ argv: ['sh', '-c', 'echo "$DISPLAY|$HOME|$EXTRA|$PWD"'], env: { EXTRA: 'a b' } });
  assert.equal(JSON.parse(out.content[0].text).stdout, `:99|${home}|a b|${home}\n`);
  const r = await run({ argv: ['pwd'], cwd: path.join(home, 'work') });
  assert.equal(r.stdout, path.join(home, 'work') + '\n');
});

test('stdin is written and closed; without it the input is empty', async () => {
  assert.equal((await run({ argv: ['cat'], stdin: 'héllo\n' })).stdout, 'héllo\n');
  assert.equal((await run({ argv: ['cat'] })).stdout, '');
  // A command that never reads its input is not an error.
  assert.equal((await run({ argv: ['true'], stdin: 'x'.repeat(200000) })).exit_code, 0);
});

test('there is no terminal', async () => {
  const r = await run({ script: 'test -t 0 || test -t 1 || test -t 2; echo $?' });
  assert.equal(r.stdout, '1\n');
});

test('arguments are checked as a whole before anything runs', async () => {
  const bad = (args, pattern) => assert.match(String(validateExecute(args, { roots })), pattern);
  bad(null, /must be an object/);
  bad([], /must be an object/);
  bad({}, /exactly one of argv/);
  bad({ argv: ['ls'], script: 'ls' }, /exactly one of argv/);
  bad({ argv: [] }, /non-empty array/);
  bad({ argv: 'ls -l' }, /non-empty array/);
  bad({ argv: ['ls', 1] }, /array of strings/);
  bad({ argv: [''] }, /argv\[0\]/);
  bad({ argv: ['ls', 'a\0b'] }, /NUL/);
  bad({ argv: Array(MAX_ARGS + 1).fill('a') }, /more than 1024/);
  bad({ argv: ['echo', 'x'.repeat(131072)] }, /larger than/);
  bad({ script: '' }, /non-empty string/);
  bad({ script: ['ls'] }, /non-empty string/);
  bad({ script: 'x'.repeat(131073) }, /larger than/);
  bad({ argv: ['ls'], command: 'rm -rf /' }, /unknown field "command"/);
  bad({ argv: ['ls'], shell: true }, /unknown field "shell"/);
  bad({ argv: ['ls'], stdin: 5 }, /stdin must be a string/);
  bad({ argv: ['ls'], stdin: 'x'.repeat(1048577) }, /stdin is larger/);
  bad({ argv: ['ls'], background: 'yes' }, /background must be/);
  bad({ argv: ['ls'], timeout_ms: 0 }, /timeout_ms must be an integer from 1 to 600000/);
  bad({ argv: ['ls'], timeout_ms: 1.5 }, /integer/);
  bad({ argv: ['ls'], timeout_ms: '100' }, /integer/);
  bad({ argv: ['ls'], timeout_ms: MAX_TIMEOUT_MS + 1 }, /from 1 to 600000/);
  bad({ argv: ['ls'], background: true, timeout_ms: MAX_BACKGROUND_TIMEOUT_MS + 1 }, /from 1 to 86400000/);
  bad({ argv: ['ls'], max_output_bytes: 0 }, /max_output_bytes/);
  bad({ argv: ['ls'], max_output_bytes: MAX_OUTPUT_BYTES + 1 }, /max_output_bytes/);
  assert.equal(validateExecute({ argv: ['ls'], timeout_ms: MAX_TIMEOUT_MS, max_output_bytes: MAX_OUTPUT_BYTES }, { roots }), null);
  assert.equal(validateExecute({ argv: ['ls'], background: true, timeout_ms: MAX_TIMEOUT_MS + 1 }, { roots }), null);

  // And the tool answers with the reason, having run nothing.
  const marker = path.join(home, 'ran');
  await refused({ script: `touch ${marker}`, argv: ['touch', marker] }, /^Error: give exactly one of/);
  await refused({ script: `touch ${marker}`, timeout_ms: -1 }, /timeout_ms/);
  assert.equal(fs.existsSync(marker), false);
});

test('cwd must be the real, normalised path of a directory inside a root', async () => {
  const bad = (cwd, pattern) => assert.match(String(cwdRefusal(cwd, roots)), pattern);
  assert.equal(cwdRefusal(home, roots), null);
  assert.equal(cwdRefusal(path.join(home, 'work'), roots), null);
  bad('work', /absolute/);
  bad(5, /absolute/);
  bad(home + '/', /normalised/);
  bad(home + '/work/..', /normalised/);
  bad(home + '/./work', /normalised/);
  bad(home + '//work', /normalised/);
  bad(path.join(home, 'missing'), /does not exist/);
  bad(path.join(home, 'file'), /not a directory/);
  // A link is refused wherever it leads: a policy reads the path as written.
  bad(path.join(home, 'link-out'), /symbolic link/);
  bad(path.join(home, 'link-in'), /symbolic link/);
  bad(elsewhere, /inside one of/);
  bad('/', /inside one of/);
  // A root's name as a prefix of another directory's is not inside it.
  fs.mkdirSync(home + '-sibling');
  try {
    bad(home + '-sibling', /inside one of/);
  } finally {
    fs.rmdirSync(home + '-sibling');
  }
  await refused({ argv: ['pwd'], cwd: path.join(home, 'link-out') }, /symbolic link/);
});

test('the roots default to the home directory, /data and /tmp, or SHELL_ROOTS', () => {
  assert.deepEqual(defaultRoots({ SHELL_ROOTS: `${home}:${path.join(home, 'missing')}:${path.join(home, 'link-out')}` }), [home, elsewhere]);
  assert.ok(defaultRoots({ HOME: home }).includes(home));
  assert.ok(defaultRoots({ HOME: home, DATA_DIR: elsewhere }).includes(elsewhere));
});

test('env cannot change what runs: PATH, the loader\'s and bash\'s variables are refused', async () => {
  for (const name of ['PATH', 'LD_PRELOAD', 'LD_LIBRARY_PATH', 'LD_AUDIT', 'BASH_ENV', 'ENV', 'BASH_FUNC_ls%%', 'SHELLOPTS', 'BASHOPTS', 'PS4', 'NODE_OPTIONS']) {
    assert.equal(envNameRefused(name), true, name);
  }
  for (const name of ['HOME', 'DISPLAY', 'LANG', 'MY_PATH', 'PATHS', 'OLD_LD_PRELOAD']) assert.equal(envNameRefused(name), false, name);
  const bad = (env, pattern) => assert.match(String(validateExecute({ argv: ['env'], env }, { roots })), pattern);
  bad({ LD_PRELOAD: '/tmp/x.so' }, /LD_PRELOAD may not be set/);
  bad({ PATH: '/tmp' }, /PATH may not be set/);
  bad({ 'A=B': 'x' }, /not a variable name/);
  bad({ '': 'x' }, /not a variable name/);
  bad({ 'BASH_FUNC_ls%%': '() { id; }' }, /not a variable name/);
  bad({ A: 1 }, /must be a string/);
  bad({ A: 'a\0b' }, /NUL/);
  bad([], /object of strings/);
  bad('A=1', /object of strings/);
  bad(Object.fromEntries(Array.from({ length: 65 }, (_, i) => [`V${i}`, ''])), /more than 64/);
  bad({ A: 'x'.repeat(32768) }, /larger than/);
  await refused({ argv: ['env'], env: { LD_PRELOAD: '/tmp/x.so' } }, /LD_PRELOAD may not be set/);
  // PATH is the server's whatever the call asks for.
  const r = await run({ argv: ['sh', '-c', 'echo "$PATH"'], env: { A: '1' } });
  assert.equal(r.stdout, process.env.PATH + '\n');
});

test('a program that does not exist is an error, not a result', async () => {
  await refused({ argv: ['no-such-program-xyz'] }, /"no-such-program-xyz" could not be started: no such program/);
  await refused({ argv: ['no-such-program-xyz'], background: true }, /could not be started/);
  assert.deepEqual((await proc({ action: 'list' })).processes, []);
});

test('at the timeout the whole process group is ended', async () => {
  const pidFile = path.join(home, 'child.pid');
  const started = Date.now();
  // The shell starts a child of its own and waits for it.
  const r = await run({ script: `sleep 60 & echo $! > ${pidFile}; wait`, timeout_ms: 400 });
  assert.equal(r.timed_out, true);
  assert.equal(r.exit_code, null);
  assert.equal(r.signal, 'SIGTERM');
  assert.ok(Date.now() - started < 5000);
  assert.ok(r.duration_ms >= 400);
  assert.ok(await goneSoon(Number(fs.readFileSync(pidFile, 'utf8'))), 'the child of the command is still running');
});

test('a command that ignores SIGTERM is killed after the grace period', async () => {
  const r = await run({ argv: [node, '-e', 'process.on("SIGTERM", () => {}); console.log("up"); setInterval(() => {}, 1000)'], timeout_ms: 2000 });
  assert.equal(r.timed_out, true);
  assert.equal(r.signal, 'SIGKILL');
  assert.equal(r.stdout, 'up\n');
});

test('what a command leaves running ends with it, and does not hold the call open', async () => {
  const pidFile = path.join(home, 'left.pid');
  const started = Date.now();
  const r = await run({ script: `sleep 60 & echo $! > ${pidFile}; echo done` });
  assert.equal(r.stdout, 'done\n');
  assert.equal(r.exit_code, 0);
  assert.ok(Date.now() - started < 5000);
  assert.ok(await goneSoon(Number(fs.readFileSync(pidFile, 'utf8'))));
});

test('output past max_output_bytes is dropped, per stream', async () => {
  const r = await run({
    argv: [node, '-e', 'process.stdout.write("a".repeat(100000)); process.stderr.write("b".repeat(10))'],
    max_output_bytes: 1000,
  });
  assert.equal(r.stdout, 'a'.repeat(1000));
  assert.equal(r.stderr, 'b'.repeat(10));
  assert.equal(r.truncated, true);
  assert.equal(r.exit_code, 0);
  // Endless output does not grow in memory or stall the command: it runs into the timeout.
  const endless = await run({ script: 'yes', max_output_bytes: 10, timeout_ms: 300 });
  assert.equal(endless.stdout, 'y\ny\ny\ny\ny\n');
  assert.deepEqual([endless.truncated, endless.timed_out], [true, true]);
  assert.equal(DEFAULT_OUTPUT_BYTES, 1048576);
});

test('output that is not UTF-8 comes back as base64; a cut character does not count', async () => {
  const r = await run({ argv: [node, '-e', 'process.stdout.write(Buffer.from([0, 255, 254, 10])); process.stderr.write("text")'] });
  assert.equal(r.stdout_encoding, 'base64');
  assert.deepEqual([...Buffer.from(r.stdout, 'base64')], [0, 255, 254, 10]);
  assert.equal(r.stderr, 'text');
  assert.equal(r.stderr_encoding, undefined);
  // "é" is two bytes: cut after the first, the result is the text before it.
  const cut = await run({ argv: [node, '-e', 'process.stdout.write("ab" + "é".repeat(10))'], max_output_bytes: 5 });
  assert.equal(cut.stdout, 'abé');
  assert.equal(cut.stdout_encoding, undefined);
  assert.equal(cut.truncated, true);

  assert.equal(incompleteTail(Buffer.from('abc')), 0);
  assert.equal(incompleteTail(Buffer.from('é')), 0);
  assert.equal(incompleteTail(Buffer.from('é').subarray(0, 1)), 1);
  assert.equal(incompleteTail(Buffer.from('€').subarray(0, 2)), 2);
  assert.equal(incompleteTail(Buffer.from('😀').subarray(0, 3)), 3);
  assert.equal(incompleteTail(Buffer.from('😀')), 0);
  assert.equal(incompleteTail(Buffer.alloc(0)), 0);
  assert.deepEqual(encodeOutput(Buffer.from('ok ✓')), { text: 'ok ✓' });
  assert.deepEqual(encodeOutput(Buffer.from([0x80])), { text: 'gA==', encoding: 'base64' });
});

test('background: a handle at once, output by polling, then the exit', async () => {
  const out = await run({
    argv: [node, '-e', 'console.log("one"); setTimeout(() => { console.log("two"); process.exit(4); }, 600)'],
    background: true,
  });
  assert.match(out.id, /^p[0-9]+$/);
  assert.equal(typeof out.pid, 'number');
  assert.ok(!Number.isNaN(Date.parse(out.started_at)));
  assert.deepEqual(Object.keys(out), ['id', 'pid', 'started_at']);

  const listed = (await proc({ action: 'list' })).processes.find((p) => p.id === out.id);
  assert.equal(listed.running, true);
  assert.equal(listed.argv[0], node);
  assert.equal(listed.cwd, home);

  let seen = '';
  let p;
  for (let i = 0; i < 20; i++) {
    p = await proc({ action: 'poll', id: out.id, wait_ms: 500 });
    seen += p.stdout;
    if (!p.running) break;
  }
  // Each poll returns only what is new.
  assert.equal(seen, 'one\ntwo\n');
  assert.deepEqual([p.running, p.exit_code, p.signal, p.timed_out], [false, 4, null, false]);
  // Its end has been read: the id is gone.
  await refused({ action: 'poll', id: out.id }, /no background command/, 'process');
});

test('background: kill ends the process group and reports it', async () => {
  const pidFile = path.join(home, 'bg.pid');
  const out = await run({ script: `sleep 60 & echo $! > ${pidFile}; echo started; wait`, background: true });
  let first;
  for (let i = 0; i < 50; i++) {
    first = await proc({ action: 'poll', id: out.id, wait_ms: 100 });
    if (first.stdout) break;
  }
  assert.equal(first.stdout, 'started\n');
  assert.equal(first.running, true);
  const killed = await proc({ action: 'kill', id: out.id });
  assert.deepEqual([killed.running, killed.exit_code, killed.signal], [false, null, 'SIGTERM']);
  assert.ok(await goneSoon(Number(fs.readFileSync(pidFile, 'utf8'))));
  assert.ok(await goneSoon(out.pid));
  await refused({ action: 'kill', id: out.id }, /no background command/, 'process');
});

test('background: a timeout applies only when given; unread output is capped', async () => {
  const timed = await run({ argv: ['sleep', '60'], background: true, timeout_ms: 300 });
  const p = await proc({ action: 'poll', id: timed.id, wait_ms: 5000 });
  assert.deepEqual([p.running, p.timed_out, p.signal], [false, true, 'SIGTERM']);

  const chatty = await run({ script: 'yes', background: true, max_output_bytes: 8 });
  await sleep(300);
  const first = await proc({ action: 'poll', id: chatty.id });
  assert.deepEqual([first.stdout, first.truncated, first.running], ['y\ny\ny\ny\n', true, true]);
  const end = await proc({ action: 'kill', id: chatty.id, signal: 'SIGKILL' });
  assert.deepEqual([end.running, end.signal], [false, 'SIGKILL']);
});

test('shell_process checks its arguments', async () => {
  const bad = (args, pattern) => assert.match(String(validateProcess(args)), pattern);
  bad(undefined, /must be an object/);
  bad({}, /action must be/);
  bad({ action: 'start' }, /action must be/);
  bad({ action: 'list', id: 'p1' }, /list takes no id/);
  bad({ action: 'poll' }, /id must be/);
  bad({ action: 'poll', id: 1 }, /id must be/);
  bad({ action: 'poll', id: 'constructor' }, /id must be/);
  bad({ action: 'poll', id: 'p1', wait_ms: 30001 }, /wait_ms/);
  bad({ action: 'poll', id: 'p1', signal: 'SIGKILL' }, /poll takes no signal/);
  bad({ action: 'kill', id: 'p1', signal: 'SIGSTOP' }, /signal must be one of/);
  bad({ action: 'kill', id: 'p1', wait_ms: 5 }, /kill takes no wait_ms/);
  bad({ action: 'kill', id: 'p1', argv: ['ls'] }, /unknown field "argv"/);
  assert.equal(validateProcess({ action: 'list' }), null);
  assert.equal(validateProcess({ action: 'poll', id: 'p12', wait_ms: 0 }), null);
  assert.equal(validateProcess({ action: 'kill', id: 'p12', signal: 'SIGINT' }), null);
  await refused({ action: 'poll', id: 'p999999' }, /no background command p999999/, 'process');
});

test('no more than the limit of commands run at a time', async () => {
  const small = createShell({ roots, home, env, maxProcesses: 2 });
  try {
    const ids = [];
    for (let i = 0; i < 2; i++) {
      const out = await small.execute({ argv: ['sleep', '30'], background: true });
      ids.push(JSON.parse(out.content[0].text).id);
    }
    const third = await small.execute({ argv: ['true'] });
    assert.equal(third.isError, true);
    assert.match(third.content[0].text, /2 commands are already running, which is the limit/);
    // Foreground commands count too, while they run.
    await small.process({ action: 'kill', id: ids[0], signal: 'SIGKILL' });
    const slow = small.execute({ argv: ['sleep', '1'] });
    await sleep(200);
    assert.equal((await small.execute({ argv: ['true'] })).isError, true);
    assert.equal(JSON.parse((await slow).content[0].text).exit_code, 0);
    assert.equal((await small.execute({ argv: ['true'] })).isError, undefined);
  } finally {
    small.stop();
  }
});

test('the tools advertise the documented arguments', () => {
  assert.equal(SHELL_TOOL.name, 'shell_execute');
  assert.equal(SHELL_PROCESS_TOOL.name, 'shell_process');
  // The policy contract's schemas (not in the image's build context).
  const dir = fileURLToPath(new URL('../../../docs/contracts/policy/tools/', import.meta.url));
  if (!fs.existsSync(dir)) return;
  for (const tool of [SHELL_TOOL, SHELL_PROCESS_TOOL]) {
    const contract = JSON.parse(fs.readFileSync(path.join(dir, `${tool.name}.schema.json`), 'utf8'));
    assert.deepEqual(Object.keys(tool.inputSchema.properties).sort(), Object.keys(contract.properties).sort());
    assert.equal(tool.inputSchema.additionalProperties, false);
    assert.equal(contract.additionalProperties, false);
    // Every documented field is one the tool accepts, and nothing else is.
    const args = tool === SHELL_TOOL ? { argv: ['true'] } : { action: 'list' };
    const validate = tool === SHELL_TOOL ? (a) => validateExecute(a, { roots }) : validateProcess;
    assert.equal(validate(args), null);
    assert.match(validate({ ...args, extra: 1 }), /unknown field "extra"/);
    for (const name of Object.keys(contract.properties)) assert.doesNotMatch(String(validate({ ...args, [name]: {} })), /unknown field/);
  }
  const c = JSON.parse(fs.readFileSync(path.join(dir, 'shell_execute.schema.json'), 'utf8')).properties;
  assert.equal(c.argv.maxItems, MAX_ARGS);
  assert.equal(c.timeout_ms.maximum, MAX_BACKGROUND_TIMEOUT_MS);
  assert.equal(c.max_output_bytes.maximum, MAX_OUTPUT_BYTES);
  assert.equal(c.max_output_bytes.default, DEFAULT_OUTPUT_BYTES);
});
