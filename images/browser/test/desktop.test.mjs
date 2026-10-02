// Usage: node --test images/browser/test/desktop.test.mjs  (needs only node)
//
// nut.js is replaced by a stand-in that records what it was asked to do, and
// the worker process by a fake one: no display is needed. What these cannot
// show is that the real addon moves the real pointer; desktop-smoke.mjs does
// that, on an Xvnc, as part of the image build.
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { createRequire } from 'node:module';
import { test } from 'node:test';
import zlib from 'node:zlib';
import {
  BUTTONS,
  DEFAULT_CONFIG,
  DESKTOP_TOOL,
  KEYS,
  MAX_OPERATIONS,
  OPERATION_NAMES,
  createDesktop,
  encodePng,
  runOperations,
  typingSteps,
  validate,
} from '../browser/desktop.js';

const op = (type, params) => ({ type, ...(params ? { params } : {}) });

// ---- validation ----

test('a pipeline using every operation is accepted', () => {
  const operations = [
    op('mouse.setPosition', { x: 1, y: 2 }),
    op('mouse.move', { x: 1, y: 2 }),
    op('mouse.getPosition'),
    op('mouse.click'),
    op('mouse.click', { button: 'RIGHT', x: 3, y: 4 }),
    op('mouse.doubleClick', { x: 3, y: 4 }),
    op('mouse.pressButton', { button: 'MIDDLE' }),
    op('mouse.releaseButton', { button: 'MIDDLE' }),
    op('mouse.drag', { from: { x: 0, y: 0 }, to: { x: 5, y: 5 } }),
    op('mouse.scrollUp', { amount: 1 }),
    op('mouse.scrollDown', { amount: 3, x: 1, y: 1 }),
    op('mouse.scrollLeft', { amount: 1 }),
    op('mouse.scrollRight', { amount: 1 }),
    op('keyboard.type', { text: 'hello' }),
    op('keyboard.type', { keys: ['Enter'] }),
    op('keyboard.pressKey', { keys: ['LeftControl', 'C'] }),
    op('keyboard.releaseKey', { keys: ['LeftControl', 'C'] }),
    op('screen.width'),
    op('screen.height'),
    op('screen.grab'),
    op('screen.grabRegion', { left: 0, top: 0, width: 10, height: 10 }),
    op('screen.colorAt', { x: 1, y: 1 }),
    op('getActiveWindow'),
    op('getWindows'),
    op('clipboard.setContent', { text: 'x' }),
    op('clipboard.getContent'),
    op('sleep', { ms: 10 }),
  ];
  assert.equal(validate({ operations, config: { keyboardDelayMs: 0, mouseDelayMs: 0, mouseSpeed: 5000 } }), null);
  // Every operation the tool advertises is in that list.
  assert.deepEqual([...new Set(operations.map((o) => o.type))].sort(), [...OPERATION_NAMES].sort());
  assert.deepEqual(DESKTOP_TOOL.inputSchema.properties.operations.items.properties.type.enum, OPERATION_NAMES);
});

test('what is wrong with a call is named, with the operation it is in', () => {
  const bad = (args, pattern) => assert.match(String(validate(args)), pattern);
  bad(undefined, /operations array is required/);
  bad({ operations: 'click' }, /operations array is required/);
  bad({ operations: [] }, /empty/);
  bad({ operations: Array(MAX_OPERATIONS + 1).fill(op('mouse.click')) }, /at most 100/);
  bad({ operations: [op('mouse.click'), 'click'] }, /operations\[1\]: must be/);
  bad({ operations: [op('mouse.teleport')] }, /operations\[0\]: unknown operation "mouse.teleport"/);
  // Names inherited from Object are not operations.
  bad({ operations: [op('constructor')] }, /unknown operation/);
  bad({ operations: [{ type: 'mouse.click', params: [] }] }, /params must be an object/);
  bad({ operations: [op('mouse.setPosition', { x: 1 })] }, /mouse.setPosition\): x and y must be integers/);
  bad({ operations: [op('mouse.setPosition', { x: 1.5, y: 2 })] }, /integers/);
  bad({ operations: [op('mouse.click', { x: 1 })] }, /both/);
  bad({ operations: [op('mouse.click', { button: 'left' })] }, /button must be one of LEFT, MIDDLE, RIGHT/);
  bad({ operations: [op('mouse.drag', { to: { x: 1 } })] }, /to\.x and y/);
  bad({ operations: [op('mouse.drag', { from: { x: 1, y: 1 } })] }, /to must be/);
  bad({ operations: [op('mouse.scrollDown', { amount: 0 })] }, /amount/);
  bad({ operations: [op('keyboard.type', {})] }, /either text or keys/);
  bad({ operations: [op('keyboard.type', { text: 'a', keys: ['A'] })] }, /either text or keys/);
  bad({ operations: [op('keyboard.type', { text: '' })] }, /non-empty/);
  bad({ operations: [op('keyboard.pressKey', { keys: ['Control'] })] }, /unknown key "Control"/);
  bad({ operations: [op('keyboard.pressKey', { keys: [] })] }, /non-empty array/);
  bad({ operations: [op('keyboard.pressKey', { keys: 'A' })] }, /non-empty array/);
  bad({ operations: [op('screen.grabRegion', { left: 0, top: 0, width: 0, height: 5 })] }, /width, height > 0/);
  bad({ operations: [op('clipboard.setContent', { text: 5 })] }, /text must be a string/);
  bad({ operations: [op('sleep', { ms: 60000 })] }, /ms must be/);
  bad({ operations: [op('mouse.click')], config: { speed: 1 } }, /unknown setting "speed"/);
  bad({ operations: [op('mouse.click')], config: { mouseSpeed: 'fast' } }, /config.mouseSpeed must be a number/);
});

test('the Key and Button names are nut.js\'s own', (t) => {
  let shared;
  try {
    // Installed only after `npm ci` in images/browser/browser.
    shared = createRequire(new URL('../browser/package.json', import.meta.url))('@nut-tree-fork/shared');
  } catch {
    t.skip('@nut-tree-fork/shared is not installed');
    return;
  }
  const names = (e) => Object.keys(e).filter((k) => Number.isNaN(Number(k)));
  assert.deepEqual(KEYS, names(shared.Key));
  assert.deepEqual(BUTTONS, names(shared.Button));
});

// ---- dispatch ----

// The parts of the nut.js module that runOperations uses. `calls` is what
// was done to the desktop, in order.
function fakeNut({ width = 1280, height = 800, failOn } = {}) {
  const calls = [];
  const did = (name, ...args) => {
    calls.push([name, ...args]);
    if (failOn === name) throw new Error(`${name} failed`);
  };
  let position = { x: 0, y: 0 };
  class Point {
    constructor(x, y) {
      Object.assign(this, { x, y });
    }
  }
  class Region {
    constructor(left, top, w, h) {
      Object.assign(this, { left, top, width: w, height: h });
    }
  }
  // 4 bytes a pixel, blue first, like libnut on X.
  const grabbed = (w, h) => ({ width: w, height: h, data: Buffer.alloc(w * h * 4, 0x80), byteWidth: w * 4, channels: 4, colorMode: 0 });
  const win = (title) => ({ title: Promise.resolve(title), region: Promise.resolve(new Region(0, 0, 640, 480)) });
  const nut = {
    calls,
    Point,
    Region,
    ColorMode: { BGR: 0, RGB: 1 },
    Button: { LEFT: 0, MIDDLE: 1, RIGHT: 2 },
    Key: Object.fromEntries(KEYS.map((k, i) => [k, i])),
    straightTo: (p) => ({ straightTo: p }),
    sleep: async (ms) => did('sleep', ms),
    mouse: {
      config: {},
      setPosition: async (p) => {
        did('setPosition', p.x, p.y);
        position = { x: p.x, y: p.y };
      },
      move: async (path) => {
        did('move', path.straightTo.x, path.straightTo.y);
        position = { ...path.straightTo };
      },
      getPosition: async () => new Point(position.x, position.y),
      click: async (b) => did('click', b),
      doubleClick: async (b) => did('doubleClick', b),
      pressButton: async (b) => did('pressButton', b),
      releaseButton: async (b) => did('releaseButton', b),
      drag: async (path) => did('drag', path.straightTo.x, path.straightTo.y),
      scrollUp: async (n) => did('scrollUp', n),
      scrollDown: async (n) => did('scrollDown', n),
      scrollLeft: async (n) => did('scrollLeft', n),
      scrollRight: async (n) => did('scrollRight', n),
    },
    keyboard: {
      config: {},
      type: async (...input) => did('type', ...input),
      pressKey: async (...k) => did('pressKey', ...k),
      releaseKey: async (...k) => did('releaseKey', ...k),
    },
    screen: {
      width: async () => width,
      height: async () => height,
      grab: async () => {
        did('grab');
        return grabbed(4, 2);
      },
      grabRegion: async (r) => {
        did('grabRegion', r.left, r.top, r.width, r.height);
        return grabbed(r.width, r.height);
      },
      colorAt: async (p) => {
        did('colorAt', p.x, p.y);
        return { R: 255, G: 0, B: 16, A: 0 };
      },
    },
    getActiveWindow: async () => win('Chromium'),
    getWindows: async () => [win('Chromium'), win('')],
    clipboard: {
      setContent: async (text) => did('setContent', text),
      getContent: async () => 'from the clipboard',
    },
  };
  return nut;
}

const K = (name) => KEYS.indexOf(name);

test('operations become the nut.js calls of the same name, in order', async () => {
  const nut = fakeNut();
  const { results, images, screen, released } = await runOperations(nut, {
    operations: [
      op('mouse.setPosition', { x: 10, y: 20 }),
      op('mouse.move', { x: 30, y: 40 }),
      op('mouse.getPosition'),
      op('mouse.click'),
      op('mouse.click', { button: 'RIGHT', x: 50, y: 60 }),
      op('mouse.doubleClick', { x: 70, y: 80 }),
      op('mouse.drag', { from: { x: 1, y: 2 }, to: { x: 3, y: 4 } }),
      op('mouse.scrollDown', { amount: 3, x: 5, y: 6 }),
      op('mouse.scrollUp', { amount: 1 }),
      op('keyboard.type', { text: 'hello' }),
      op('keyboard.type', { keys: ['Tab', 'Enter'] }),
      op('keyboard.pressKey', { keys: ['LeftControl', 'A'] }),
      op('keyboard.releaseKey', { keys: ['LeftControl', 'A'] }),
      op('clipboard.setContent', { text: 'héllo' }),
      op('clipboard.getContent'),
      op('screen.width'),
      op('screen.height'),
      op('screen.colorAt', { x: 7, y: 8 }),
      op('getActiveWindow'),
      op('getWindows'),
      op('sleep', { ms: 5 }),
    ],
  });
  assert.deepEqual(nut.calls, [
    ['setPosition', 10, 20],
    ['move', 30, 40],
    ['click', 0],
    ['setPosition', 50, 60],
    ['click', 2],
    ['setPosition', 70, 80],
    ['doubleClick', 0],
    ['setPosition', 1, 2],
    ['drag', 3, 4],
    ['setPosition', 5, 6],
    ['scrollDown', 3],
    ['scrollUp', 1],
    ['type', 'hello'],
    ['type', K('Tab'), K('Enter')],
    ['pressKey', K('LeftControl'), K('A')],
    ['releaseKey', K('LeftControl'), K('A')],
    ['setContent', 'héllo'],
    ['colorAt', 7, 8],
    ['sleep', 5],
  ]);
  assert.ok(results.every((r) => r.success));
  const result = (i) => results[i].result;
  assert.deepEqual(result(2), { x: 30, y: 40 });
  assert.deepEqual(result(4), { clicked: 'RIGHT', x: 50, y: 60 });
  assert.deepEqual(result(14), { text: 'from the clipboard' });
  assert.equal(result(15), 1280);
  assert.equal(result(16), 800);
  assert.deepEqual(result(17), { R: 255, G: 0, B: 16, hex: '#ff0010' });
  assert.deepEqual(result(18), { title: 'Chromium', region: { left: 0, top: 0, width: 640, height: 480 } });
  assert.equal(result(19).length, 2);
  assert.deepEqual(images, []);
  assert.deepEqual(screen, { width: 1280, height: 800 });
  // Everything pressed was released by the pipeline itself.
  assert.equal(released, undefined);
});

test('typed text: characters that need Shift are pressed as keys, the rest goes to nut.js as it is', async () => {
  assert.deepEqual(typingSteps('Hi there'), [{ text: 'Hi there' }]);
  assert.deepEqual(typingSteps('a!b'), [{ text: 'a' }, { shifted: 'Num1' }, { text: 'b' }]);
  assert.deepEqual(typingSteps('x\r\n\ty'), [{ text: 'x' }, { key: 'Enter' }, { key: 'Tab' }, { text: 'y' }]);
  // Every shifted symbol of a US layout has a key.
  for (const ch of '~!@#$%^&*()_+{}|:"<>?') {
    const [step] = typingSteps(ch);
    assert.ok(KEYS.includes(step.shifted), ch);
  }
  // Names inherited from Object are not symbols.
  assert.deepEqual(typingSteps('constructor'), [{ text: 'constructor' }]);

  const nut = fakeNut();
  const out = await runOperations(nut, { operations: [op('keyboard.type', { text: 'Hi!?\n' })] });
  assert.deepEqual(nut.calls, [
    ['type', 'Hi'],
    ['pressKey', K('LeftShift'), K('Num1')],
    ['releaseKey', K('LeftShift'), K('Num1')],
    ['pressKey', K('LeftShift'), K('Slash')],
    ['releaseKey', K('LeftShift'), K('Slash')],
    ['type', K('Enter')],
  ]);
  assert.deepEqual(out.results[0].result, { typed: '5 chars' });
  assert.equal(out.released, undefined);

  // Shift is let go even when the key under it failed.
  const failing = fakeNut({ failOn: 'releaseKey' });
  const failed = await runOperations(failing, { operations: [op('keyboard.type', { text: '!' })] });
  assert.equal(failed.results[0].success, false);
  assert.deepEqual(failing.calls.map((c) => c[0]), ['pressKey', 'releaseKey', 'grab', 'releaseKey']);
});

test('the delays are the defaults here unless the call sets them', async () => {
  const nut = fakeNut();
  await runOperations(nut, { operations: [op('mouse.getPosition')] });
  assert.equal(nut.keyboard.config.autoDelayMs, DEFAULT_CONFIG.keyboardDelayMs);
  assert.equal(nut.mouse.config.autoDelayMs, DEFAULT_CONFIG.mouseDelayMs);
  assert.equal(nut.mouse.config.mouseSpeed, DEFAULT_CONFIG.mouseSpeed);
  await runOperations(nut, { operations: [op('mouse.getPosition')], config: { keyboardDelayMs: 0, mouseSpeed: 500 } });
  assert.equal(nut.keyboard.config.autoDelayMs, 0);
  assert.equal(nut.mouse.config.autoDelayMs, DEFAULT_CONFIG.mouseDelayMs);
  assert.equal(nut.mouse.config.mouseSpeed, 500);
});

test('screenshots come back as PNGs, numbered in the order taken', async () => {
  const nut = fakeNut();
  const { results, images } = await runOperations(nut, {
    operations: [op('screen.grab'), op('screen.grabRegion', { left: 1, top: 2, width: 3, height: 5 })],
  });
  assert.deepEqual(results.map((r) => r.result), [
    { image_index: 0, width: 4, height: 2 },
    { image_index: 1, width: 3, height: 5 },
  ]);
  assert.deepEqual(nut.calls, [['grab'], ['grabRegion', 1, 2, 3, 5]]);
  for (const [i, [w, h]] of [[4, 2], [3, 5]].entries()) {
    const png = Buffer.from(images[i], 'base64');
    assert.equal(png.subarray(1, 4).toString('latin1'), 'PNG');
    assert.equal(png.readUInt32BE(16), w);
    assert.equal(png.readUInt32BE(20), h);
  }
});

test('the size given by the caller wins over nut.js\'s, which may be out of date', async () => {
  const nut = fakeNut({ width: 1280, height: 800 });
  const size = { width: 800, height: 600 };
  const { results, screen } = await runOperations(
    nut,
    { operations: [op('screen.width'), op('screen.height'), op('mouse.setPosition', { x: 1000, y: 100 })] },
    { size },
  );
  assert.deepEqual(screen, size);
  assert.equal(results[0].result, 800);
  assert.equal(results[1].result, 600);
  assert.equal(results[2].success, false);
  assert.match(results[2].error, /\(1000, 100\) is outside the 800x600 screen/);
});

test('a point or region outside the screen fails before nut.js is asked', async () => {
  for (const operation of [
    op('mouse.setPosition', { x: 1280, y: 0 }),
    op('mouse.move', { x: 0, y: 800 }),
    op('mouse.click', { x: -1, y: 5 }),
    op('mouse.drag', { to: { x: 5000, y: 5 } }),
    op('mouse.drag', { from: { x: 5000, y: 5 }, to: { x: 1, y: 1 } }),
    op('screen.colorAt', { x: 1280, y: 800 }),
    op('screen.grabRegion', { left: 1000, top: 0, width: 281, height: 10 }),
  ]) {
    const nut = fakeNut();
    const { results } = await runOperations(nut, { operations: [operation] });
    assert.equal(results[0].success, false, operation.type);
    assert.match(results[0].error, /outside the 1280x800 screen/);
    // Only the screenshot of the failure.
    assert.deepEqual(nut.calls, [['grab']]);
  }
});

test('the pipeline stops at the first failure and attaches a screenshot', async () => {
  const nut = fakeNut({ failOn: 'click' });
  const { results, images } = await runOperations(nut, {
    operations: [op('mouse.setPosition', { x: 1, y: 1 }), op('mouse.click'), op('keyboard.type', { text: 'never typed' })],
  });
  assert.deepEqual(results.map((r) => r.success), [true, false]);
  assert.equal(results[1].operation, 'mouse.click');
  assert.equal(results[1].error, 'click failed');
  assert.equal(images.length, 1);
  assert.deepEqual(nut.calls.map((c) => c[0]), ['setPosition', 'click', 'grab']);
});

test('nut.js rejecting with a string is still an error message', async () => {
  const nut = fakeNut();
  nut.screen.grab = async () => Promise.reject('Unable to fetch screen content.');
  const { results, images } = await runOperations(nut, { operations: [op('screen.grab')] });
  assert.equal(results[0].error, 'Unable to fetch screen content.');
  assert.deepEqual(images, []);
});

test('keys and buttons still held when the call ends are released', async () => {
  // Left held by the pipeline.
  let nut = fakeNut();
  let out = await runOperations(nut, {
    operations: [op('keyboard.pressKey', { keys: ['LeftControl', 'LeftShift'] }), op('mouse.pressButton'), op('keyboard.releaseKey', { keys: ['LeftShift'] })],
  });
  assert.deepEqual(out.released, ['LeftControl', 'LEFT']);
  assert.deepEqual(nut.calls.slice(-2), [['releaseKey', K('LeftControl')], ['releaseButton', 0]]);

  // Held when a later operation failed.
  nut = fakeNut({ failOn: 'type' });
  out = await runOperations(nut, {
    operations: [op('keyboard.pressKey', { keys: ['LeftAlt'] }), op('keyboard.type', { text: 'x' }), op('keyboard.releaseKey', { keys: ['LeftAlt'] })],
  });
  assert.deepEqual(out.results.map((r) => r.success), [true, false]);
  assert.deepEqual(out.released, ['LeftAlt']);

  // A drag that failed half way leaves LEFT down.
  nut = fakeNut({ failOn: 'drag' });
  out = await runOperations(nut, { operations: [op('mouse.drag', { to: { x: 5, y: 5 } })] });
  assert.deepEqual(out.released, ['LEFT']);
});

// ---- PNG ----

test('the PNG holds the pixels, red and blue swapped back, without the padding', () => {
  // 2x2, 4 bytes a pixel (B, G, R, pad), rows padded to 12 bytes.
  const data = Buffer.from([
    1, 2, 3, 0, /**/ 4, 5, 6, 0, /**/ 99, 99, 99, 99,
    7, 8, 9, 0, /**/ 10, 11, 12, 0, /**/ 99, 99, 99, 99,
  ]);
  const png = encodePng({ width: 2, height: 2, data, byteWidth: 12 });
  assert.deepEqual([...png.subarray(0, 8)], [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

  const chunks = [];
  for (let at = 8; at < png.length; ) {
    const length = png.readUInt32BE(at);
    const type = png.subarray(at + 4, at + 8).toString('latin1');
    const body = png.subarray(at + 8, at + 8 + length);
    assert.equal(png.readUInt32BE(at + 8 + length), zlib.crc32(png.subarray(at + 4, at + 8 + length)), `${type} checksum`);
    chunks.push({ type, body });
    at += 12 + length;
  }
  assert.deepEqual(chunks.map((c) => c.type), ['IHDR', 'IDAT', 'IEND']);
  const header = chunks[0].body;
  assert.equal(header.readUInt32BE(0), 2);
  assert.equal(header.readUInt32BE(4), 2);
  assert.deepEqual([...header.subarray(8)], [8, 2, 0, 0, 0]); // 8-bit RGB: no alpha
  assert.deepEqual([...zlib.inflateSync(chunks[1].body)], [0, 3, 2, 1, 6, 5, 4, 0, 9, 8, 7, 12, 11, 10]);

  // Already red first: left as it is.
  const rgb = encodePng({ width: 1, height: 1, data: Buffer.from([1, 2, 3, 0]), byteWidth: 4, bgr: false });
  assert.deepEqual([...zlib.inflateSync(rgb.subarray(8 + 25 + 8, rgb.length - 12 - 4))], [0, 1, 2, 3]);
});

// ---- the tool: worker lifecycle ----

// Stands in for the forked worker. Answers each message with `answer(msg)`
// unless told to stay silent.
function fakeWorkers({ answer = (msg) => ({ results: [{ success: true, operation: msg.operations[0].type, result: 1 }], images: [], screen: msg.size }) } = {}) {
  const started = [];
  const start = () => {
    const child = new EventEmitter();
    child.received = [];
    child.killed = false;
    child.silent = false;
    child.kill = () => {
      child.killed = true;
    };
    child.send = (msg, cb) => {
      child.received.push(msg);
      cb?.(null);
      if (!child.silent) setImmediate(() => child.emit('message', { id: msg.id, ...answer(msg) }));
    };
    started.push(child);
    return child;
  };
  return { started, start };
}

const call = { operations: [op('mouse.getPosition')] };

test('a call returns JSON text, and screenshots as image content', async () => {
  const { start, started } = fakeWorkers({
    answer: () => ({ results: [{ success: true, operation: 'screen.grab', result: { image_index: 0 } }], images: ['QUJD'], screen: { width: 1280, height: 800 } }),
  });
  const desktop = createDesktop({ start, getSize: async () => ({ width: 1280, height: 800 }) });
  const out = await desktop({ operations: [op('screen.grab')], config: { mouseSpeed: 500 } });
  assert.equal(out.isError, undefined);
  assert.deepEqual(JSON.parse(out.content[0].text), {
    results: [{ success: true, operation: 'screen.grab', result: { image_index: 0 } }],
    screen: { width: 1280, height: 800 },
  });
  assert.deepEqual(out.content[1], { type: 'image', data: 'QUJD', mimeType: 'image/png' });
  assert.deepEqual(started[0].received, [{ id: 1, size: { width: 1280, height: 800 }, operations: [op('screen.grab')], config: { mouseSpeed: 500 } }]);
});

test('a failed pipeline is an error result that says where it failed', async () => {
  const { start } = fakeWorkers({
    answer: () => ({
      results: [{ success: true, operation: 'mouse.setPosition', result: {} }, { success: false, operation: 'mouse.click', error: 'boom' }],
      images: ['QUJD'],
      screen: { width: 1, height: 1 },
    }),
  });
  const out = await createDesktop({ start, getSize: async () => null })(call);
  assert.equal(out.isError, true);
  assert.match(out.content[0].text, /^Desktop pipeline failed at operation 1 \("mouse.click"\): boom\n\n\{/);
  assert.equal(out.content[1].type, 'image');
});

test('an invalid call is refused without starting the worker', async () => {
  const { start, started } = fakeWorkers();
  const out = await createDesktop({ start, getSize: async () => null })({ operations: [op('mouse.click', { button: 'left' })] });
  assert.equal(out.isError, true);
  assert.match(out.content[0].text, /^Error: operations\[0\] \(mouse.click\): button must be one of/);
  assert.equal(started.length, 0);
});

test('one worker serves call after call, and is replaced when the screen was resized', async () => {
  const { start, started } = fakeWorkers();
  let size = { width: 1280, height: 800 };
  const desktop = createDesktop({ start, getSize: async () => size });
  await desktop(call);
  await desktop(call);
  assert.equal(started.length, 1);
  assert.equal(started[0].received.length, 2);

  size = { width: 1920, height: 1080 };
  const out = await desktop(call);
  assert.equal(started.length, 2);
  assert.equal(started[0].killed, true);
  assert.deepEqual(JSON.parse(out.content[0].text).screen, size);
  assert.deepEqual(started[1].received[0].size, size);
});

test('calls run one at a time', async () => {
  const { start, started } = fakeWorkers();
  const desktop = createDesktop({ start, getSize: async () => null });
  const order = [];
  const first = desktop(call).then(() => order.push('first'));
  const second = desktop(call).then(() => order.push('second'));
  await new Promise((resolve) => setImmediate(resolve));
  // The second message is not sent until the first was answered.
  assert.equal(started[0].received.length, 1);
  await Promise.all([first, second]);
  assert.deepEqual(order, ['first', 'second']);
  assert.equal(started[0].received.length, 2);
});

test('a worker that dies mid-call is an error, and the next call gets a new one', async () => {
  const { start, started } = fakeWorkers();
  const desktop = createDesktop({ start, getSize: async () => null });
  await desktop(call);
  started[0].silent = true;
  const pending = desktop(call);
  await new Promise((resolve) => setImmediate(resolve));
  started[0].emit('exit', 1, null);
  const out = await pending;
  assert.equal(out.isError, true);
  assert.match(out.content[0].text, /the desktop worker exited \(code 1\)/);

  const next = await desktop(call);
  assert.equal(next.isError, undefined);
  assert.equal(started.length, 2);
});

test('a worker that died between calls is replaced', async () => {
  const { start, started } = fakeWorkers();
  const desktop = createDesktop({ start, getSize: async () => null });
  await desktop(call);
  started[0].emit('exit', null, 'SIGKILL');
  assert.equal((await desktop(call)).isError, undefined);
  assert.equal(started.length, 2);
});

test('an error reported by the worker (nut.js failed to load) is the result', async () => {
  const { start } = fakeWorkers({ answer: () => ({ error: 'nut.js could not be loaded (linux/arm64, DISPLAY=:99): no native build' }) });
  const out = await createDesktop({ start, getSize: async () => null })(call);
  assert.equal(out.isError, true);
  assert.equal(out.content[0].text, 'Error: nut.js could not be loaded (linux/arm64, DISPLAY=:99): no native build');
});

test('a call that never answers is stopped, and its worker killed', async () => {
  const { start, started } = fakeWorkers();
  const desktop = createDesktop({ start, getSize: async () => null, timeoutMs: 20 });
  await desktop(call);
  started[0].silent = true;
  const out = await desktop(call);
  assert.equal(out.isError, true);
  assert.match(out.content[0].text, /took longer than 0.02 s/);
  assert.equal(started[0].killed, true);
  assert.equal((await desktop(call)).isError, undefined);
  assert.equal(started.length, 2);
});
