// The desktop_execute tool against a real X display: the packaged nut.js
// addon, the worker process, xdpyinfo and xsel. Run by the flake's
// `desktop-smoke` check (Xvnc and openbox already up on $DISPLAY), which the
// image build depends on. Not a `node --test` file: it needs that display.
//
//   DESKTOP_JS=<path to the installed desktop.js> node desktop-smoke.mjs
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const { createDesktop } = await import(pathToFileURL(process.env.DESKTOP_JS).href);
const desktop = createDesktop();
const op = (type, params) => ({ type, ...(params ? { params } : {}) });
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Runs a pipeline that must succeed; returns each operation's result and the
// call's images.
async function run(label, operations) {
  const started = Date.now();
  const out = await desktop({ operations });
  const text = out.content[0].text;
  assert.ok(!out.isError, `${label}: ${text}`);
  const body = JSON.parse(text);
  console.log(`ok   ${label} (${Date.now() - started} ms)`);
  return {
    results: body.results.map((r) => r.result),
    screen: body.screen,
    images: out.content.slice(1).map((c) => Buffer.from(c.data, 'base64')),
  };
}

const pngSize = (png) => {
  assert.equal(png.subarray(1, 4).toString('latin1'), 'PNG');
  return { width: png.readUInt32BE(16), height: png.readUInt32BE(20) };
};

// A terminal that writes the first line typed into it to a file. openbox
// maximises and focuses it.
const typedFile = path.join(os.tmpdir(), `desktop-smoke-typed-${process.pid}`);
const xterm = spawn('xterm', ['-fa', 'DejaVu Sans Mono', '-fs', '12', '-T', 'smoke-terminal', '-e', 'sh', '-c', `IFS= read -r line; printf %s "$line" > ${typedFile}`], {
  stdio: 'inherit',
});
await sleep(2000);

try {
  // Screen and pointer.
  let r = await run('screen size, pointer set and read back', [
    op('screen.width'),
    op('screen.height'),
    op('mouse.setPosition', { x: 100, y: 120 }),
    op('mouse.getPosition'),
    op('mouse.move', { x: 300, y: 200 }),
    op('mouse.getPosition'),
  ]);
  assert.deepEqual(r.screen, { width: 1280, height: 800 });
  assert.deepEqual(r.results.slice(0, 2), [1280, 800]);
  assert.deepEqual(r.results[3], { x: 100, y: 120 });
  assert.deepEqual(r.results[5], { x: 300, y: 200 });

  // Screenshots.
  r = await run('screen.grab, grabRegion, colorAt', [
    op('screen.grab'),
    op('screen.grabRegion', { left: 10, top: 20, width: 200, height: 100 }),
    op('screen.colorAt', { x: 640, y: 400 }),
  ]);
  assert.deepEqual(pngSize(r.images[0]), { width: 1280, height: 800 });
  assert.deepEqual(pngSize(r.images[1]), { width: 200, height: 100 });
  assert.match(r.results[2].hex, /^#[0-9a-f]{6}$/);
  console.log(`     full screenshot: ${r.images[0].length} bytes; colour at the centre: ${r.results[2].hex}`);

  // Keyboard, into a real client: click the terminal, type, press Enter.
  const text = 'Hello, desktop 123!';
  r = await run('click, keyboard.type (text and keys), key combination', [
    op('mouse.click', { x: 640, y: 400 }),
    op('keyboard.type', { text }),
    op('keyboard.pressKey', { keys: ['LeftShift', 'A'] }),
    op('keyboard.releaseKey', { keys: ['LeftShift', 'A'] }),
    op('keyboard.type', { keys: ['Enter'] }),
  ]);
  for (let i = 0; i < 50 && !fs.existsSync(typedFile); i++) await sleep(100);
  assert.equal(fs.readFileSync(typedFile, 'utf8'), text + 'A');
  console.log(`     the terminal received ${JSON.stringify(text + 'A')}`);

  // The rest of the mouse: nothing to observe, but X must accept each.
  await run('double click, buttons, drag, scroll', [
    op('mouse.doubleClick', { x: 200, y: 200 }),
    op('mouse.click', { button: 'RIGHT', x: 5, y: 5 }),
    op('keyboard.type', { keys: ['Escape'] }),
    op('mouse.pressButton', { button: 'MIDDLE' }),
    op('mouse.releaseButton', { button: 'MIDDLE' }),
    op('mouse.drag', { from: { x: 300, y: 300 }, to: { x: 400, y: 350 } }),
    op('mouse.scrollDown', { amount: 2 }),
    op('mouse.scrollUp', { amount: 2 }),
    op('mouse.scrollLeft', { amount: 1 }),
    op('mouse.scrollRight', { amount: 1 }),
  ]);

  // Clipboard, through xsel.
  r = await run('clipboard set and read back', [op('clipboard.setContent', { text: 'smoke ✓ clipboard' }), op('clipboard.getContent')]);
  assert.deepEqual(r.results[1], { text: 'smoke ✓ clipboard' });

  // A viewer resizes the desktop: the next call must see the new size, and
  // must not capture with the old one.
  for (const [width, height] of [[1024, 768], [1920, 1080]]) {
    execFileSync('xrandr', ['-s', `${width}x${height}`], { stdio: 'inherit' });
    await sleep(500);
    r = await run(`after a resize to ${width}x${height}`, [
      op('screen.width'),
      op('screen.height'),
      op('screen.grab'),
      op('mouse.setPosition', { x: width - 1, y: height - 1 }),
      op('mouse.getPosition'),
    ]);
    assert.deepEqual(r.screen, { width, height });
    assert.deepEqual(r.results.slice(0, 2), [width, height]);
    assert.deepEqual(pngSize(r.images[0]), { width, height });
    assert.deepEqual(r.results[4], { x: width - 1, y: height - 1 });
  }

  // A point outside the screen is an error result, not a crash.
  const outside = await desktop({ operations: [op('mouse.click', { x: 5000, y: 5 })] });
  assert.equal(outside.isError, true);
  assert.match(outside.content[0].text, /outside the 1920x1080 screen/);
  console.log('ok   a point outside the screen is refused');

  // Windows. Reported, not asserted: what nut.js sees depends on the window
  // manager (openbox reparents windows into frames of its own).
  spawn('xterm', ['-fa', 'DejaVu Sans Mono', '-T', 'smoke-window', '-e', 'sleep', '30'], { stdio: 'inherit' });
  await sleep(2000);
  const windows = await desktop({ operations: [op('mouse.click', { x: 640, y: 400 }), op('getActiveWindow'), op('getWindows')] });
  console.log(`info windows: ${windows.content[0].text.replace(/\s+/g, ' ')}`);

  console.log('desktop smoke test passed');
} finally {
  desktop.stop();
  xterm.kill();
}
process.exit(0);
