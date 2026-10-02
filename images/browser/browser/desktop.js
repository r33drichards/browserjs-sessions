// Desktop control: the `desktop_execute` MCP tool.
//
// browser_execute drives pages over CDP. This drives the X display itself
// (mouse, keyboard, screen, clipboard) with nut.js, so a caller can do what a
// person at the VNC view can: browser chrome, dialogs, file choosers, other
// windows. Operations mirror the nut.js API one to one.
//
// nut.js runs in a child process (desktop-worker.js), not in the server:
// - its native addon (libnut) talks to X through Xlib, where any X error
//   (a window that went away, a capture across a resize) ends the process;
// - libnut reads the screen size once per connection, and the desktop is
//   resized whenever a viewer resizes its window, so the worker is replaced
//   when the size has changed;
// - encoding a screenshot is CPU work that would stall every other request.

import { execFile, fork } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import zlib from 'node:zlib';

export const BUTTONS = ['LEFT', 'MIDDLE', 'RIGHT'];

// The names of nut.js's `Key` enum (desktop.test.mjs compares the two).
// prettier-ignore
export const KEYS = [
  'Escape', 'F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12', 'F13', 'F14', 'F15', 'F16',
  'F17', 'F18', 'F19', 'F20', 'F21', 'F22', 'F23', 'F24', 'Print', 'ScrollLock', 'Pause', 'Grave', 'Num1', 'Num2',
  'Num3', 'Num4', 'Num5', 'Num6', 'Num7', 'Num8', 'Num9', 'Num0', 'Minus', 'Equal', 'Backspace', 'Insert', 'Home',
  'PageUp', 'NumLock', 'NumPadEqual', 'Divide', 'Multiply', 'Subtract', 'Tab', 'Q', 'W', 'E', 'R', 'T', 'Y', 'U',
  'I', 'O', 'P', 'LeftBracket', 'RightBracket', 'Backslash', 'Delete', 'End', 'PageDown', 'NumPad7', 'NumPad8',
  'NumPad9', 'Add', 'CapsLock', 'A', 'S', 'D', 'F', 'G', 'H', 'J', 'K', 'L', 'Semicolon', 'Quote', 'Return',
  'NumPad4', 'NumPad5', 'NumPad6', 'LeftShift', 'Z', 'X', 'C', 'V', 'B', 'N', 'M', 'Comma', 'Period', 'Slash',
  'RightShift', 'Up', 'NumPad1', 'NumPad2', 'NumPad3', 'Enter', 'LeftControl', 'LeftSuper', 'LeftWin', 'LeftCmd',
  'LeftAlt', 'LeftMeta', 'RightControl', 'RightSuper', 'RightWin', 'RightAlt', 'RightCmd', 'RightMeta', 'Space',
  'Menu', 'Fn', 'Left', 'Down', 'Right', 'NumPad0', 'Decimal', 'Clear', 'AudioMute', 'AudioVolDown', 'AudioVolUp',
  'AudioPlay', 'AudioStop', 'AudioPause', 'AudioPrev', 'AudioNext', 'AudioRewind', 'AudioForward', 'AudioRepeat',
  'AudioRandom',
];

export const MAX_OPERATIONS = 100;
export const MAX_TEXT = 10000;
export const MAX_SLEEP_MS = 30000;
export const MAX_SCROLL = 100;
const MAX_KEYS = 8;

// nut.js waits 300 ms before every key and 100 ms before every mouse action
// by default: far too slow for typed text. These are the defaults here; a
// call may override them with `config`.
export const DEFAULT_CONFIG = { keyboardDelayMs: 10, mouseDelayMs: 50, mouseSpeed: 2000 };
const CONFIG_LIMITS = { keyboardDelayMs: [0, 1000], mouseDelayMs: [0, 1000], mouseSpeed: [100, 20000] };

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const isInt = (v) => Number.isInteger(v);

// Checks of one operation's params, by kind. Each returns what is wrong, or
// nothing. Bounds against the screen are checked when the operation runs:
// only then is the size known.
const point = (p, what = '') => (isInt(p.x) && isInt(p.y) ? null : `${what}x and y must be integers`);
const optionalPoint = (p) => {
  if (p.x === undefined && p.y === undefined) return null;
  return isInt(p.x) && isInt(p.y) ? null : 'x and y must both be integers (or both be left out)';
};
const button = (p) =>
  p.button === undefined || BUTTONS.includes(p.button) ? null : `button must be one of ${BUTTONS.join(', ')}`;
const keys = (p) => {
  if (!Array.isArray(p.keys) || p.keys.length === 0) return 'keys must be a non-empty array of Key names';
  if (p.keys.length > MAX_KEYS) return `at most ${MAX_KEYS} keys at a time`;
  const unknown = p.keys.find((k) => !KEYS.includes(k));
  return unknown === undefined ? null : `unknown key ${JSON.stringify(unknown)} (see the Key names in the tool description)`;
};
const none = () => null;
const all = (...checks) => (p) => checks.map((c) => c(p)).find(Boolean) || null;
const scroll = all((p) => (isInt(p.amount) && p.amount >= 1 && p.amount <= MAX_SCROLL ? null : `amount must be an integer from 1 to ${MAX_SCROLL}`), optionalPoint);

const OPERATIONS = {
  'mouse.setPosition': point,
  'mouse.move': point,
  'mouse.getPosition': none,
  'mouse.click': all(button, optionalPoint),
  'mouse.doubleClick': all(button, optionalPoint),
  'mouse.pressButton': button,
  'mouse.releaseButton': button,
  'mouse.drag': (p) => {
    if (!isObject(p.to)) return 'to must be { x, y }';
    if (p.from !== undefined && !isObject(p.from)) return 'from must be { x, y }';
    return point(p.to, 'to.') || (p.from ? point(p.from, 'from.') : null);
  },
  'mouse.scrollUp': scroll,
  'mouse.scrollDown': scroll,
  'mouse.scrollLeft': scroll,
  'mouse.scrollRight': scroll,
  'keyboard.type': (p) => {
    if ((p.text === undefined) === (p.keys === undefined)) return 'give either text or keys';
    if (p.keys !== undefined) return keys(p);
    if (typeof p.text !== 'string' || p.text.length === 0) return 'text must be a non-empty string';
    return p.text.length > MAX_TEXT ? `text is longer than ${MAX_TEXT} characters` : null;
  },
  'keyboard.pressKey': keys,
  'keyboard.releaseKey': keys,
  'screen.width': none,
  'screen.height': none,
  'screen.grab': none,
  'screen.grabRegion': (p) =>
    [p.left, p.top, p.width, p.height].every(isInt) && p.width > 0 && p.height > 0 && p.left >= 0 && p.top >= 0
      ? null
      : 'left, top, width and height must be integers (left, top >= 0; width, height > 0)',
  'screen.colorAt': point,
  getActiveWindow: none,
  getWindows: none,
  'clipboard.setContent': (p) =>
    typeof p.text === 'string' && p.text.length <= MAX_TEXT ? null : `text must be a string of at most ${MAX_TEXT} characters`,
  'clipboard.getContent': none,
  sleep: (p) => (isInt(p.ms) && p.ms >= 0 && p.ms <= MAX_SLEEP_MS ? null : `ms must be an integer from 0 to ${MAX_SLEEP_MS}`),
};

export const OPERATION_NAMES = Object.keys(OPERATIONS);

// The whole call is checked before anything runs: half a pipeline of clicks
// and keystrokes is worse than none. Returns what is wrong, or null.
export function validate(args) {
  if (!isObject(args) || !Array.isArray(args.operations)) return 'operations array is required';
  if (args.operations.length === 0) return 'operations is empty';
  if (args.operations.length > MAX_OPERATIONS) return `at most ${MAX_OPERATIONS} operations per call`;
  for (const [i, op] of args.operations.entries()) {
    const where = `operations[${i}]`;
    if (!isObject(op) || typeof op.type !== 'string') return `${where}: must be { type, params? }`;
    if (!Object.hasOwn(OPERATIONS, op.type)) return `${where}: unknown operation ${JSON.stringify(op.type)}`;
    if (op.params !== undefined && !isObject(op.params)) return `${where} (${op.type}): params must be an object`;
    const wrong = OPERATIONS[op.type](op.params || {});
    if (wrong) return `${where} (${op.type}): ${wrong}`;
  }
  if (args.config !== undefined) {
    if (!isObject(args.config)) return 'config must be an object';
    for (const [name, value] of Object.entries(args.config)) {
      const limits = CONFIG_LIMITS[name];
      if (!limits) return `config: unknown setting ${JSON.stringify(name)} (${Object.keys(CONFIG_LIMITS).join(', ')})`;
      if (typeof value !== 'number' || !(value >= limits[0] && value <= limits[1])) {
        return `config.${name} must be a number from ${limits[0]} to ${limits[1]}`;
      }
    }
  }
  return null;
}

// A PNG (8-bit RGB, no alpha) from what nut.js's screen.grab() returns: rows
// of `byteWidth` bytes, 4 bytes a pixel, blue first. The fourth byte is X's
// padding, not opacity (it is zero on many servers), so it is dropped
// instead of being written as an alpha channel.
export function encodePng({ width, height, data, byteWidth, bytesPerPixel = 4, bgr = true }) {
  const row = 1 + width * 3;
  const raw = Buffer.alloc(row * height);
  const [r, b] = bgr ? [2, 0] : [0, 2];
  for (let y = 0; y < height; y++) {
    let src = y * byteWidth;
    let dst = y * row + 1; // after the row's filter byte (0: none)
    for (let x = 0; x < width; x++, src += bytesPerPixel, dst += 3) {
      raw[dst] = data[src + r];
      raw[dst + 1] = data[src + 1];
      raw[dst + 2] = data[src + b];
    }
  }
  const chunk = (type, body) => {
    const out = Buffer.alloc(12 + body.length);
    out.writeUInt32BE(body.length, 0);
    out.write(type, 4, 'latin1');
    body.copy(out, 8);
    out.writeUInt32BE(zlib.crc32(out.subarray(4, 8 + body.length)), 8 + body.length);
    return out;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header.set([8, 2, 0, 0, 0], 8); // 8 bits a channel, RGB
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', header),
    chunk('IDAT', zlib.deflateSync(raw, { level: 6 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

const MAX_IMAGES = 10;

// Runs a validated call against nut.js (`nut` is the module, or a stand-in
// with the same shape). `size` is the display's size now, when the caller
// knows it; nut.js's own answer is as old as its X connection.
//
// Stops at the first failing operation. Whatever the pipeline left held down
// (keys, mouse buttons) is released before returning, failed or not: a
// modifier stuck down would garble everything the person at the VNC view and
// the next call do.
export async function runOperations(nut, { operations, config = {} }, { size, png = encodePng } = {}) {
  const settings = { ...DEFAULT_CONFIG, ...config };
  nut.keyboard.config.autoDelayMs = settings.keyboardDelayMs;
  nut.mouse.config.autoDelayMs = settings.mouseDelayMs;
  nut.mouse.config.mouseSpeed = settings.mouseSpeed;

  const screen = size || { width: await nut.screen.width(), height: await nut.screen.height() };
  const at = (p, what = '') => {
    if (p.x < 0 || p.y < 0 || p.x >= screen.width || p.y >= screen.height) {
      throw new Error(`${what}(${p.x}, ${p.y}) is outside the ${screen.width}x${screen.height} screen`);
    }
    return new nut.Point(p.x, p.y);
  };
  const moveIfGiven = async (p) => {
    if (p.x !== undefined) await nut.mouse.setPosition(at(p));
  };
  const buttonOf = (p) => nut.Button[p.button || 'LEFT'];
  const keysOf = (p) => p.keys.map((k) => nut.Key[k]);
  const plainPoint = (p) => ({ x: p.x, y: p.y });
  const plainRegion = (r) => ({ left: r.left, top: r.top, width: r.width, height: r.height });
  const describe = async (win) => ({ title: await win.title, region: plainRegion(await win.region) });

  const images = [];
  const image = async (grabbed) => {
    if (images.length >= MAX_IMAGES) throw new Error(`at most ${MAX_IMAGES} screenshots per call`);
    const data = await png({
      width: grabbed.width,
      height: grabbed.height,
      data: grabbed.data,
      byteWidth: grabbed.byteWidth,
      bytesPerPixel: grabbed.channels || 4,
      bgr: grabbed.colorMode === nut.ColorMode.BGR,
    });
    images.push(Buffer.from(data).toString('base64'));
    return { image_index: images.length - 1, width: grabbed.width, height: grabbed.height };
  };

  const heldKeys = new Set();
  const heldButtons = new Set();

  const run = {
    'mouse.setPosition': async (p) => {
      await nut.mouse.setPosition(at(p));
      return plainPoint(p);
    },
    'mouse.move': async (p) => {
      await nut.mouse.move(nut.straightTo(at(p)));
      return plainPoint(p);
    },
    'mouse.getPosition': async () => plainPoint(await nut.mouse.getPosition()),
    'mouse.click': async (p) => {
      await moveIfGiven(p);
      await nut.mouse.click(buttonOf(p));
      return { clicked: p.button || 'LEFT', ...plainPoint(await nut.mouse.getPosition()) };
    },
    'mouse.doubleClick': async (p) => {
      await moveIfGiven(p);
      await nut.mouse.doubleClick(buttonOf(p));
      return { doubleClicked: p.button || 'LEFT', ...plainPoint(await nut.mouse.getPosition()) };
    },
    'mouse.pressButton': async (p) => {
      heldButtons.add(p.button || 'LEFT');
      await nut.mouse.pressButton(buttonOf(p));
      return { pressed: p.button || 'LEFT' };
    },
    'mouse.releaseButton': async (p) => {
      await nut.mouse.releaseButton(buttonOf(p));
      heldButtons.delete(p.button || 'LEFT');
      return { released: p.button || 'LEFT' };
    },
    'mouse.drag': async (p) => {
      const to = at(p.to, 'to ');
      if (p.from) await nut.mouse.setPosition(at(p.from, 'from '));
      // nut.js presses LEFT, moves along the path, and releases.
      heldButtons.add('LEFT');
      await nut.mouse.drag(nut.straightTo(to));
      heldButtons.delete('LEFT');
      return { dragged_to: plainPoint(p.to) };
    },
    'keyboard.type': async (p) => {
      if (p.text !== undefined) {
        await nut.keyboard.type(p.text);
        return { typed: p.text.length + ' chars' };
      }
      await nut.keyboard.type(...keysOf(p));
      return { typed: p.keys };
    },
    'keyboard.pressKey': async (p) => {
      for (const k of p.keys) heldKeys.add(k);
      await nut.keyboard.pressKey(...keysOf(p));
      return { pressed: p.keys };
    },
    'keyboard.releaseKey': async (p) => {
      await nut.keyboard.releaseKey(...keysOf(p));
      for (const k of p.keys) heldKeys.delete(k);
      return { released: p.keys };
    },
    'screen.width': async () => screen.width,
    'screen.height': async () => screen.height,
    'screen.grab': async () => image(await nut.screen.grab()),
    'screen.grabRegion': async (p) => {
      if (p.left + p.width > screen.width || p.top + p.height > screen.height) {
        throw new Error(`the region reaches outside the ${screen.width}x${screen.height} screen`);
      }
      return image(await nut.screen.grabRegion(new nut.Region(p.left, p.top, p.width, p.height)));
    },
    'screen.colorAt': async (p) => {
      const c = await nut.screen.colorAt(at(p));
      const hex = '#' + [c.R, c.G, c.B].map((v) => v.toString(16).padStart(2, '0')).join('');
      return { R: c.R, G: c.G, B: c.B, hex };
    },
    getActiveWindow: async () => describe(await nut.getActiveWindow()),
    getWindows: async () => Promise.all((await nut.getWindows()).map(describe)),
    'clipboard.setContent': async (p) => {
      await nut.clipboard.setContent(p.text);
      return { copied: p.text.length + ' chars' };
    },
    'clipboard.getContent': async () => ({ text: await nut.clipboard.getContent() }),
    sleep: async (p) => {
      await nut.sleep(p.ms);
      return { slept_ms: p.ms };
    },
  };
  for (const name of ['scrollUp', 'scrollDown', 'scrollLeft', 'scrollRight']) {
    run[`mouse.${name}`] = async (p) => {
      await moveIfGiven(p);
      await nut.mouse[name](p.amount);
      return { scrolled: p.amount };
    };
  }

  const results = [];
  for (const op of operations) {
    try {
      results.push({ success: true, operation: op.type, result: await run[op.type](op.params || {}) });
    } catch (err) {
      results.push({ success: false, operation: op.type, error: messageOf(err) });
      // What the screen looked like when it failed, like browser_execute.
      try {
        await image(await nut.screen.grab());
      } catch {}
      break;
    }
  }

  const released = [];
  if (heldKeys.size) {
    const names = [...heldKeys].reverse();
    await nut.keyboard.releaseKey(...names.map((k) => nut.Key[k])).then(() => released.push(...names), () => {});
  }
  for (const name of heldButtons) {
    await nut.mouse.releaseButton(nut.Button[name]).then(() => released.push(name), () => {});
  }
  return { results, images, screen, ...(released.length ? { released } : {}) };
}

// nut.js rejects with strings as well as Errors.
const messageOf = (err) => (err && err.message) || String(err);

// The X display's size now, from xdpyinfo (an X connection of its own, so
// never out of date). Null when it cannot be read.
export function displaySize() {
  return new Promise((resolve) => {
    execFile('xdpyinfo', [], { timeout: 5000 }, (err, stdout) => {
      const m = err ? null : /dimensions:\s*(\d+)x(\d+) pixels/.exec(stdout);
      resolve(m ? { width: Number(m[1]), height: Number(m[2]) } : null);
    });
  });
}

const WORKER = fileURLToPath(new URL('./desktop-worker.js', import.meta.url));
const startWorker = () => fork(WORKER, [], { stdio: ['ignore', 'inherit', 'inherit', 'ipc'] });

// The tool, as server.js calls it: `await desktop(args)` returns an MCP tool
// result. One call at a time (there is one mouse and one keyboard). The
// worker starts on the first call and is replaced when the screen size has
// changed since it started or when a call outlives `timeoutMs`.
export function createDesktop({ start = startWorker, getSize = displaySize, timeoutMs = 120000 } = {}) {
  let worker = null; // { child, sizeKey }
  let tail = Promise.resolve();
  let nextId = 1;

  function stop() {
    if (!worker) return;
    worker.child.kill('SIGKILL');
    worker = null;
  }

  function send(size, args) {
    const sizeKey = size ? `${size.width}x${size.height}` : '';
    if (worker && worker.sizeKey !== sizeKey) stop();
    if (!worker) {
      const started = start();
      worker = { child: started, sizeKey };
      // Also between calls: forget a worker that died, and never let its
      // 'error' event go unhandled (that would end the server).
      const forget = () => {
        if (worker && worker.child === started) worker = null;
      };
      started.once('exit', forget);
      started.on('error', forget);
    }
    const { child } = worker;
    const id = nextId++;
    return new Promise((resolve, reject) => {
      const done = (fn, value) => {
        clearTimeout(timer);
        child.off('message', onMessage);
        child.off('exit', onExit);
        child.off('error', onError);
        fn(value);
      };
      const gone = () => {
        if (worker && worker.child === child) worker = null;
      };
      const onMessage = (msg) => {
        if (!msg || msg.id !== id) return;
        if (msg.error) done(reject, new Error(msg.error));
        else done(resolve, msg);
      };
      const onExit = (code, signal) => {
        gone();
        done(
          reject,
          new Error(
            `the desktop worker exited (${signal || 'code ' + code}) while running this call: the X server reported an error, ` +
              'for example a window that closed or a screen resized under it. Operations before that did run; look at the screen before retrying.',
          ),
        );
      };
      const onError = (err) => {
        gone();
        done(reject, new Error(`the desktop worker could not be started: ${messageOf(err)}`));
      };
      const timer = setTimeout(() => {
        gone();
        child.kill('SIGKILL');
        done(reject, new Error(`the call took longer than ${timeoutMs / 1000} s and was stopped`));
      }, timeoutMs);
      child.on('message', onMessage);
      child.once('exit', onExit);
      child.once('error', onError);
      child.send({ id, size, operations: args.operations, config: args.config }, (err) => {
        if (err) onError(err);
      });
    });
  }

  function desktop(args) {
    const wrong = validate(args);
    if (wrong) return Promise.resolve(errorResult(`Error: ${wrong}`));
    const run = tail.then(async () => {
      try {
        const { results, images, screen, released } = await send(await getSize(), args);
        const failed = results.find((r) => !r.success);
        const body = JSON.stringify({ results, screen, ...(released ? { released } : {}) }, null, 2);
        return {
          content: [
            {
              type: 'text',
              text: failed ? `Desktop pipeline failed at operation ${results.length - 1} ("${failed.operation}"): ${failed.error}\n\n${body}` : body,
            },
            ...images.map((data) => ({ type: 'image', data, mimeType: 'image/png' })),
          ],
          ...(failed ? { isError: true } : {}),
        };
      } catch (err) {
        return errorResult(`Error: ${messageOf(err)}`);
      }
    });
    tail = run;
    return run;
  }
  desktop.stop = stop;
  return desktop;
}

const errorResult = (text) => ({ content: [{ type: 'text', text }], isError: true });

export const DESKTOP_TOOL = {
  name: 'desktop_execute',
  description:
    'Drive the whole desktop (not just web pages) with nut.js: the mouse, the keyboard, the screen and the clipboard of ' +
    'the X display that Chromium runs on and that the person watches over VNC. Use it for what browser_execute cannot ' +
    'reach: browser chrome and dialogs, file choosers, permission prompts, anything outside the page. For page content ' +
    'prefer browser_execute, which is faster and addresses elements by selector instead of by pixel.\n\n' +
    'Operations run in order; each mirrors the nut.js call of the same name:\n' +
    '- mouse.setPosition: { x, y } (jump there)\n' +
    '- mouse.move: { x, y } (glide there in a straight line)\n' +
    '- mouse.getPosition: {} -> { x, y }\n' +
    '- mouse.click / mouse.doubleClick: { button?, x?, y? } (at x, y when given, else where the pointer is)\n' +
    '- mouse.pressButton / mouse.releaseButton: { button? }\n' +
    '- mouse.drag: { to: { x, y }, from?: { x, y } } (LEFT button held from `from`, or the current position, to `to`)\n' +
    '- mouse.scrollUp / mouse.scrollDown / mouse.scrollLeft / mouse.scrollRight: { amount, x?, y? } (wheel steps)\n' +
    '- keyboard.type: { text } (types the string) or { keys: [Key, ...] } (taps each key in turn)\n' +
    '- keyboard.pressKey / keyboard.releaseKey: { keys: [Key, ...] } (hold and let go: a combination is pressKey then releaseKey with the same keys)\n' +
    '- screen.width / screen.height: {} -> pixels\n' +
    '- screen.grab: {} (the whole screen) / screen.grabRegion: { left, top, width, height } (PNG, returned as image content)\n' +
    '- screen.colorAt: { x, y } -> { R, G, B, hex }\n' +
    '- getActiveWindow: {} -> { title, region } / getWindows: {} -> [{ title, region }]\n' +
    '- clipboard.setContent: { text } / clipboard.getContent: {} -> { text }\n' +
    '- sleep: { ms }\n\n' +
    `Button: ${BUTTONS.join(', ')} (default LEFT). Key: ${KEYS.join(', ')}.\n\n` +
    'Coordinates are screen pixels from the top left corner. The screen is resized when a viewer resizes their window, ' +
    'so read `screen` from a result (or take a screenshot) before aiming at coordinates. ' +
    'The result text is JSON: { results: [{ success, operation, result | error }], screen: { width, height } }; a ' +
    'screenshot\'s result holds image_index, its position among the image content items that follow the text. ' +
    'The call is checked as a whole before anything runs, stops at the first operation that fails (attaching a ' +
    'screenshot of that state), and releases any key or button still held when it ends. ' +
    `config: { keyboardDelayMs?, mouseDelayMs?, mouseSpeed? } (defaults ${DEFAULT_CONFIG.keyboardDelayMs}, ${DEFAULT_CONFIG.mouseDelayMs}, ${DEFAULT_CONFIG.mouseSpeed} px/s).`,
  inputSchema: {
    type: 'object',
    properties: {
      operations: {
        type: 'array',
        items: {
          type: 'object',
          properties: {
            type: { type: 'string', enum: OPERATION_NAMES },
            params: { type: 'object' },
          },
          required: ['type'],
        },
      },
      config: {
        type: 'object',
        description: 'Delays for this call: keyboardDelayMs (before each key), mouseDelayMs (before each mouse action), mouseSpeed (px/s for move and drag).',
        properties: {
          keyboardDelayMs: { type: 'number' },
          mouseDelayMs: { type: 'number' },
          mouseSpeed: { type: 'number' },
        },
      },
    },
    required: ['operations'],
  },
};
