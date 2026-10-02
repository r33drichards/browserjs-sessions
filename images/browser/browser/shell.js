// Commands on the desktop: the `shell_execute` and `shell_process` MCP tools.
//
// browser_execute drives pages and desktop_execute the display; these run
// programs, as the user the desktop runs as, with the environment this server
// was started in (PATH, HOME, DISPLAY): what a person gets in a terminal on
// the desktop, same files, and a command can open a window there.
//
// The arguments are shaped for the policies that judge them before they get
// here (docs/contracts/policy/shell-execute-input.md):
// - `argv` is executed as given, with no shell in between, so the program
//   and each argument a policy read are the program and arguments that run;
// - `script` is handed to `bash -lc`, and is a string to a policy;
// - nothing else names a command: unknown fields are refused, `env` cannot
//   change PATH or what the loader and bash load, and `cwd` must be written
//   as the real path of the directory (normalised, no symbolic links).
//
// Nothing here confines a command. It can do what the container lets its
// user do (gVisor, uid 1000, no capabilities, the pod's NetworkPolicy), and
// no less: `cwd` roots and the refused variables keep the arguments honest,
// they are not a sandbox.

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export const DEFAULT_TIMEOUT_MS = 30000;
export const MAX_TIMEOUT_MS = 600000;
export const MAX_BACKGROUND_TIMEOUT_MS = 86400000;
export const DEFAULT_OUTPUT_BYTES = 1048576;
export const MAX_OUTPUT_BYTES = 4194304;
export const MAX_ARGS = 1024;
export const MAX_COMMAND_BYTES = 131072;
export const MAX_STDIN_BYTES = 1048576;
export const MAX_ENV_VARS = 64;
export const MAX_ENV_BYTES = 32768;
export const MAX_POLL_WAIT_MS = 30000;
export const MAX_PROCESSES = 16;
// Finished background commands whose result nobody has read yet.
const MAX_FINISHED = 64;
export const SIGNALS = ['SIGTERM', 'SIGKILL', 'SIGINT', 'SIGHUP'];

// Variables a call may not set: they change which program a name in `argv`
// means, or make the loader, bash or node run code of the caller's choosing
// before the program a policy allowed.
const DENIED_ENV = new Set(['PATH', 'BASH_ENV', 'ENV', 'SHELLOPTS', 'BASHOPTS', 'PS4', 'NODE_OPTIONS']);
const DENIED_ENV_PREFIXES = ['LD_', 'BASH_FUNC_'];
export const envNameRefused = (name) => DENIED_ENV.has(name) || DENIED_ENV_PREFIXES.some((p) => name.startsWith(p));

const EXECUTE_FIELDS = ['argv', 'script', 'cwd', 'env', 'stdin', 'timeout_ms', 'max_output_bytes', 'background'];
const PROCESS_FIELDS = ['action', 'id', 'wait_ms', 'signal'];

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const bytes = (s) => Buffer.byteLength(s, 'utf8');

function unknownField(args, known) {
  const extra = Object.keys(args).find((k) => !known.includes(k));
  return extra === undefined ? null : `unknown field ${JSON.stringify(extra)} (known: ${known.join(', ')})`;
}

function integerIn(args, name, min, max) {
  const v = args[name];
  if (v === undefined) return null;
  if (!Number.isInteger(v) || v < min || v > max) return `${name} must be an integer from ${min} to ${max}`;
  return null;
}

// The roots a `cwd` may be in, as real paths; ones that do not exist are
// dropped. SHELL_ROOTS (colon separated) replaces the default.
export function defaultRoots(env = process.env) {
  const listed = env.SHELL_ROOTS ? env.SHELL_ROOTS.split(':') : [env.HOME || os.homedir(), env.DATA_DIR || '/data', '/tmp'];
  const roots = [];
  for (const dir of listed) {
    try {
      if (dir) roots.push(fs.realpathSync(dir));
    } catch {}
  }
  return [...new Set(roots)];
}

const inside = (dir, root) => dir === root || dir.startsWith(root === '/' ? '/' : root + '/');

// Why this `cwd` is refused, or null.
export function cwdRefusal(cwd, roots) {
  if (typeof cwd !== 'string' || !cwd.startsWith('/')) return 'cwd must be an absolute path';
  if (cwd.includes('\0') || bytes(cwd) > 4096) return 'cwd is not a usable path';
  if (path.posix.normalize(cwd) !== cwd || (cwd.length > 1 && cwd.endsWith('/'))) {
    return 'cwd must be written normalised: no ".", "..", "//" or trailing "/"';
  }
  let real;
  try {
    real = fs.realpathSync(cwd);
    if (!fs.statSync(real).isDirectory()) return `cwd ${JSON.stringify(cwd)} is not a directory`;
  } catch {
    return `cwd ${JSON.stringify(cwd)} does not exist`;
  }
  if (real !== cwd) return `cwd passes through a symbolic link; name the directory itself (${JSON.stringify(real)})`;
  if (!roots.some((root) => inside(cwd, root))) return `cwd must be inside one of: ${roots.join(', ')}`;
  return null;
}

// Why these shell_execute arguments are refused, or null. Everything is
// checked before anything runs.
export function validateExecute(args, { roots = defaultRoots() } = {}) {
  if (!isObject(args)) return 'arguments must be an object';
  const extra = unknownField(args, EXECUTE_FIELDS);
  if (extra) return extra;

  const hasArgv = args.argv !== undefined;
  const hasScript = args.script !== undefined;
  if (hasArgv === hasScript) return 'give exactly one of argv (a program and its arguments) and script (bash source)';
  if (hasArgv) {
    const { argv } = args;
    if (!Array.isArray(argv) || argv.length === 0) return 'argv must be a non-empty array of strings';
    if (argv.length > MAX_ARGS) return `argv has more than ${MAX_ARGS} items`;
    if (!argv.every((a) => typeof a === 'string')) return 'argv must be a non-empty array of strings';
    if (argv[0] === '') return 'argv[0], the program, is empty';
    if (argv.some((a) => a.includes('\0'))) return 'argv must not contain NUL characters';
    if (argv.reduce((n, a) => n + bytes(a) + 1, 0) > MAX_COMMAND_BYTES) return `argv is larger than ${MAX_COMMAND_BYTES} bytes`;
  } else {
    const { script } = args;
    if (typeof script !== 'string' || script === '') return 'script must be a non-empty string';
    if (script.includes('\0')) return 'script must not contain NUL characters';
    if (bytes(script) > MAX_COMMAND_BYTES) return `script is larger than ${MAX_COMMAND_BYTES} bytes`;
  }

  if (args.cwd !== undefined) {
    const wrong = cwdRefusal(args.cwd, roots);
    if (wrong) return wrong;
  }

  if (args.env !== undefined) {
    if (!isObject(args.env)) return 'env must be an object of strings';
    const names = Object.keys(args.env);
    if (names.length > MAX_ENV_VARS) return `env has more than ${MAX_ENV_VARS} variables`;
    let size = 0;
    for (const name of names) {
      const value = args.env[name];
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) return `env: ${JSON.stringify(name)} is not a variable name`;
      if (envNameRefused(name)) return `env: ${name} may not be set`;
      if (typeof value !== 'string' || value.includes('\0')) return `env: the value of ${name} must be a string without NUL characters`;
      size += bytes(name) + bytes(value) + 2;
    }
    if (size > MAX_ENV_BYTES) return `env is larger than ${MAX_ENV_BYTES} bytes`;
  }

  if (args.stdin !== undefined) {
    if (typeof args.stdin !== 'string') return 'stdin must be a string';
    if (bytes(args.stdin) > MAX_STDIN_BYTES) return `stdin is larger than ${MAX_STDIN_BYTES} bytes`;
  }
  if (args.background !== undefined && typeof args.background !== 'boolean') return 'background must be true or false';
  return (
    integerIn(args, 'timeout_ms', 1, args.background ? MAX_BACKGROUND_TIMEOUT_MS : MAX_TIMEOUT_MS) ||
    integerIn(args, 'max_output_bytes', 1, MAX_OUTPUT_BYTES)
  );
}

export function validateProcess(args) {
  if (!isObject(args)) return 'arguments must be an object';
  const extra = unknownField(args, PROCESS_FIELDS);
  if (extra) return extra;
  const { action } = args;
  if (!['list', 'poll', 'kill'].includes(action)) return 'action must be "list", "poll" or "kill"';
  if (action === 'list') {
    const given = ['id', 'wait_ms', 'signal'].find((k) => args[k] !== undefined);
    return given ? `list takes no ${given}` : null;
  }
  if (typeof args.id !== 'string' || !/^p[0-9]+$/.test(args.id)) return 'id must be the id shell_execute returned ("p1", "p2", ...)';
  if (action === 'poll') {
    if (args.signal !== undefined) return 'poll takes no signal';
    return integerIn(args, 'wait_ms', 0, MAX_POLL_WAIT_MS);
  }
  if (args.wait_ms !== undefined) return 'kill takes no wait_ms';
  if (args.signal !== undefined && !SIGNALS.includes(args.signal)) return `signal must be one of ${SIGNALS.join(', ')}`;
  return null;
}

// How many bytes at the end of `buf` are the start of a UTF-8 character whose
// rest has not arrived (or was cut off).
export function incompleteTail(buf) {
  for (let i = buf.length - 1; i >= 0 && i >= buf.length - 4; i--) {
    const b = buf[i];
    if ((b & 0xc0) === 0x80) continue;
    const need = b >= 0xf0 ? 4 : b >= 0xe0 ? 3 : b >= 0xc0 ? 2 : 1;
    return buf.length - i < need ? buf.length - i : 0;
  }
  return 0;
}

const utf8 = new TextDecoder('utf-8', { fatal: true });

// Output as a string: the text when it is valid UTF-8, else base64.
export function encodeOutput(buf) {
  try {
    return { text: utf8.decode(buf) };
  } catch {
    return { text: buf.toString('base64'), encoding: 'base64' };
  }
}

const text = (t) => ({ content: [{ type: 'text', text: t }] });
const errorResult = (t) => ({ content: [{ type: 'text', text: `Error: ${t}` }], isError: true });
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// The tools, as server.js calls them: `await shell.execute(args)` and
// `await shell.process(args)` return MCP tool results.
export function createShell({
  roots = defaultRoots(),
  home = process.env.HOME || os.homedir(),
  env = process.env,
  maxProcesses = Number(process.env.SHELL_MAX_PROCESSES) || MAX_PROCESSES,
  killGraceMs = 2000,
} = {}) {
  const running = new Set(); // every command that has not ended
  const background = new Map(); // id -> command, until its end has been read
  let nextId = 1;

  // The whole group: the command is its leader (`detached`), so this reaches
  // what it started too, unless that moved itself to a session of its own.
  function killGroup(cmd, signal) {
    try {
      process.kill(-cmd.child.pid, signal);
    } catch {
      try {
        cmd.child.kill(signal);
      } catch {}
    }
  }

  function start(args) {
    const stream = () => ({ chunks: [], bytes: 0, truncated: false });
    const cmd = {
      id: null,
      command: args.argv ? { argv: args.argv } : { script: args.script },
      cwd: args.cwd ?? home,
      max: args.max_output_bytes ?? DEFAULT_OUTPUT_BYTES,
      stdout: stream(),
      stderr: stream(),
      startedAt: Date.now(),
      endedAt: null,
      running: true,
      exitCode: null,
      signal: null,
      timedOut: false,
      error: null,
      timers: [],
    };
    const [file, argv] = args.argv ? [args.argv[0], args.argv.slice(1)] : ['bash', ['-lc', args.script]];
    const child = spawn(file, argv, {
      cwd: cmd.cwd,
      env: { ...env, ...args.env },
      // A session and process group of its own, and no terminal.
      detached: true,
      stdio: [args.stdin === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
    });
    cmd.child = child;
    running.add(cmd);

    let finish;
    cmd.done = new Promise((resolve) => (finish = resolve));
    const end = () => {
      if (!cmd.running) return;
      cmd.running = false;
      cmd.endedAt = Date.now();
      for (const t of cmd.timers) clearTimeout(t);
      child.stdout?.destroy();
      child.stderr?.destroy();
      running.delete(cmd);
      finish();
    };

    cmd.spawned = new Promise((resolve) => {
      child.once('spawn', resolve);
      child.once('error', (err) => {
        cmd.error =
          err.code === 'ENOENT'
            ? `${JSON.stringify(file)} could not be started: no such program (looked up on PATH unless it is a path)`
            : `${JSON.stringify(file)} could not be started: ${err.message || err}`;
        end();
        resolve();
      });
    });

    for (const name of ['stdout', 'stderr']) {
      const s = cmd[name];
      child[name].on('data', (chunk) => {
        const room = cmd.max - s.bytes;
        if (chunk.length > room) {
          s.truncated = true;
          chunk = chunk.subarray(0, room);
        }
        if (chunk.length) {
          s.chunks.push(chunk);
          s.bytes += chunk.length;
        }
      });
      child[name].on('error', () => {});
    }
    if (child.stdin) {
      // A command that exits without reading its input: not an error.
      child.stdin.on('error', () => {});
      child.stdin.end(args.stdin);
    }

    child.once('exit', (code, signal) => {
      cmd.exitCode = code;
      cmd.signal = signal;
      // What it left running goes with it: only a command started with
      // `background: true` outlives its call. Then, shortly, stop waiting for
      // output: a process that escaped the group may hold the pipes open.
      killGroup(cmd, 'SIGKILL');
      cmd.timers.push(setTimeout(end, 250));
    });
    child.once('close', end);

    const timeout = args.timeout_ms ?? (args.background ? null : DEFAULT_TIMEOUT_MS);
    if (timeout !== null) {
      cmd.timers.push(
        setTimeout(() => {
          cmd.timedOut = true;
          killGroup(cmd, 'SIGTERM');
          cmd.timers.push(setTimeout(() => killGroup(cmd, 'SIGKILL'), killGraceMs));
        }, timeout),
      );
    }
    return cmd;
  }

  // What the command wrote since the last time this was called, and how it
  // stands.
  function report(cmd) {
    const out = {};
    let truncated = false;
    for (const name of ['stdout', 'stderr']) {
      const s = cmd[name];
      let buf = Buffer.concat(s.chunks);
      s.chunks = [];
      s.bytes = 0;
      // A character split by the cut, or one whose rest is still to come
      // (kept for the next poll), is not what makes output binary.
      const tail = incompleteTail(buf);
      if (tail && (s.truncated || cmd.running)) {
        if (cmd.running && !s.truncated) {
          s.chunks = [buf.subarray(buf.length - tail)];
          s.bytes = tail;
        }
        buf = buf.subarray(0, buf.length - tail);
      }
      const { text: value, encoding } = encodeOutput(buf);
      out[name] = value;
      if (encoding) out[`${name}_encoding`] = encoding;
      truncated ||= s.truncated;
      s.truncated = false;
    }
    return {
      exit_code: cmd.exitCode,
      signal: cmd.signal,
      stdout: out.stdout,
      stderr: out.stderr,
      ...(out.stdout_encoding ? { stdout_encoding: out.stdout_encoding } : {}),
      ...(out.stderr_encoding ? { stderr_encoding: out.stderr_encoding } : {}),
      truncated,
      duration_ms: (cmd.endedAt ?? Date.now()) - cmd.startedAt,
      timed_out: cmd.timedOut,
    };
  }

  const status = (cmd) => ({ id: cmd.id, running: cmd.running, ...report(cmd) });

  async function execute(args = {}) {
    const wrong = validateExecute(args, { roots });
    if (wrong) return errorResult(wrong);
    if (running.size >= maxProcesses) {
      return errorResult(`${running.size} commands are already running, which is the limit; wait for one or end one with shell_process`);
    }
    let cmd;
    try {
      cmd = start(args);
    } catch (err) {
      return errorResult(`the command could not be started: ${err.message || err}`);
    }
    await cmd.spawned;
    if (cmd.error) return errorResult(cmd.error);

    if (args.background) {
      // Forget the oldest unread results before they pile up.
      const finished = [...background.values()].filter((c) => !c.running);
      for (const old of finished.slice(0, Math.max(0, finished.length - MAX_FINISHED + 1))) background.delete(old.id);
      cmd.id = `p${nextId++}`;
      background.set(cmd.id, cmd);
      return text(JSON.stringify({ id: cmd.id, pid: cmd.child.pid, started_at: new Date(cmd.startedAt).toISOString() }));
    }
    await cmd.done;
    return text(JSON.stringify(report(cmd)));
  }

  async function processTool(args = {}) {
    const wrong = validateProcess(args);
    if (wrong) return errorResult(wrong);
    if (args.action === 'list') {
      const processes = [...background.values()].map((cmd) => ({
        id: cmd.id,
        pid: cmd.child.pid,
        ...cmd.command,
        cwd: cmd.cwd,
        running: cmd.running,
        started_at: new Date(cmd.startedAt).toISOString(),
        exit_code: cmd.exitCode,
        signal: cmd.signal,
      }));
      return text(JSON.stringify({ processes }));
    }
    const cmd = background.get(args.id);
    if (!cmd) {
      return errorResult(`no background command ${args.id}: there never was one, or it ended and its result has been read`);
    }
    if (args.action === 'kill') {
      if (cmd.running) killGroup(cmd, args.signal ?? 'SIGTERM');
      await Promise.race([cmd.done, sleep(killGraceMs)]);
    } else if (args.wait_ms) {
      let timer;
      await Promise.race([cmd.done, new Promise((resolve) => (timer = setTimeout(resolve, args.wait_ms)))]);
      clearTimeout(timer);
    }
    const result = status(cmd);
    if (!cmd.running) background.delete(cmd.id);
    return text(JSON.stringify(result));
  }

  // Ends everything still running (tests, and the server on its way out).
  function stop() {
    for (const cmd of running) killGroup(cmd, 'SIGKILL');
  }

  return { execute, process: processTool, stop };
}

export const SHELL_TOOL = {
  name: 'shell_execute',
  description:
    'Run a command on the desktop the browser runs on: same user, HOME, files and PATH as a terminal there, and DISPLAY ' +
    'is set, so a command can open a window the person watching sees. Give exactly one of:\n' +
    '- argv: ["program", "arg", ...] executed directly, with no shell (no quoting to get wrong, no expansion). Prefer this.\n' +
    '- script: "..." run as `bash -lc <script>`, for pipes, redirection, globs and loops.\n\n' +
    'Options: cwd (an absolute, normalised path with no symbolic links, inside the home directory, /data or /tmp; ' +
    'default the home directory), env ({ NAME: "value" } added to the environment; PATH, LD_*, BASH_ENV, ENV, ' +
    'BASH_FUNC_*, SHELLOPTS, BASHOPTS, PS4 and NODE_OPTIONS are refused), stdin (a string; without it the input is ' +
    `/dev/null), timeout_ms (default ${DEFAULT_TIMEOUT_MS}, at most ${MAX_TIMEOUT_MS}), max_output_bytes (per stream, ` +
    `default ${DEFAULT_OUTPUT_BYTES}, at most ${MAX_OUTPUT_BYTES}), background.\n\n` +
    'The result text is JSON: { exit_code, signal, stdout, stderr, truncated, duration_ms, timed_out }. A command that ' +
    'exits with a non-zero code is a result, not an error: check exit_code. stdout and stderr are text; a stream that ' +
    'is not valid UTF-8 comes back base64 encoded, marked by stdout_encoding or stderr_encoding: "base64". Output past ' +
    'max_output_bytes is dropped and truncated is true. At the timeout the command and everything it started are sent ' +
    'SIGTERM, then SIGKILL two seconds later, and timed_out is true. There is no terminal (no TTY), and when the ' +
    'command exits, whatever it left running in the background is ended with it.\n\n' +
    'For something that keeps running (a server, a GUI application, a long build) pass background: true: the call ' +
    'returns { id, pid, started_at } at once; read its output and end it with shell_process. In the background there ' +
    `is no timeout unless timeout_ms is given (at most ${MAX_BACKGROUND_TIMEOUT_MS}). At most ${MAX_PROCESSES} commands run at a time.`,
  inputSchema: {
    type: 'object',
    additionalProperties: false,
    properties: {
      argv: {
        type: 'array',
        items: { type: 'string' },
        minItems: 1,
        maxItems: MAX_ARGS,
        description: 'The program and its arguments, executed directly with no shell.',
      },
      script: { type: 'string', minLength: 1, maxLength: MAX_COMMAND_BYTES, description: 'Shell source, run as bash -lc <script>.' },
      cwd: { type: 'string', description: 'Working directory (default: the home directory).' },
      env: { type: 'object', additionalProperties: { type: 'string' }, description: 'Variables added to the environment.' },
      stdin: { type: 'string', description: "Written to the command's standard input." },
      timeout_ms: { type: 'integer', minimum: 1, maximum: MAX_BACKGROUND_TIMEOUT_MS },
      max_output_bytes: { type: 'integer', minimum: 1, maximum: MAX_OUTPUT_BYTES },
      background: { type: 'boolean', description: 'Start it and return a handle for shell_process.' },
    },
  },
};

export const SHELL_PROCESS_TOOL = {
  name: 'shell_process',
  description:
    'Follow and end commands that shell_execute started with background: true.\n' +
    '- { action: "list" } -> { processes: [{ id, pid, argv | script, cwd, running, started_at, exit_code, signal }] }\n' +
    `- { action: "poll", id, wait_ms? } -> the output written since the previous poll and the state: { id, running, ` +
    `exit_code, signal, stdout, stderr, truncated, duration_ms, timed_out }. wait_ms (at most ${MAX_POLL_WAIT_MS}) waits ` +
    'that long for the command to exit before answering.\n' +
    `- { action: "kill", id, signal? } -> sends the signal (${SIGNALS.join(', ')}; default SIGTERM) to the command and ` +
    'everything it started, waits up to two seconds, and answers like poll.\n\n' +
    'Unread output is kept up to max_output_bytes per stream; past that it is dropped and truncated is true, so poll a ' +
    'chatty command regularly. Once a poll or kill has reported running: false the id is forgotten.',
  inputSchema: {
    type: 'object',
    additionalProperties: false,
    properties: {
      action: { type: 'string', enum: ['list', 'poll', 'kill'] },
      id: { type: 'string', description: 'The id shell_execute returned (poll and kill).' },
      wait_ms: { type: 'integer', minimum: 0, maximum: MAX_POLL_WAIT_MS },
      signal: { type: 'string', enum: SIGNALS },
    },
    required: ['action'],
  },
};
