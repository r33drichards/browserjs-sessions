// Usage: node --test computer-use-mcp/test/clipboard.test.mjs  (needs only node; X is mocked)
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { after, before, test } from 'node:test';
import { ClipboardUnavailable, createClipboard, uriList } from '../browser/clipboard.js';
import { createFiles } from '../browser/files.js';

// xclip, as far as clipboard.js can tell: it takes what is written to it and
// stays until it loses the selection (lose) or is killed.
function fakeXclip({ failWith } = {}) {
  const runs = [];
  const spawn = (cmd, args) => {
    const child = new EventEmitter();
    Object.assign(child, { cmd, args, input: '', killed: false, alive: true });
    child.stdin = Object.assign(new EventEmitter(), { end: (data) => (child.input = data) });
    child.lose = (code = 0) => {
      if (!child.alive) return;
      child.alive = false;
      child.emit('exit', code);
    };
    child.kill = () => {
      child.killed = true;
      setImmediate(() => child.lose(null));
    };
    runs.push(child);
    if (failWith === 'enoent') setImmediate(() => child.emit('error', new Error('spawn xclip ENOENT')));
    if (failWith === 'exit') setImmediate(() => child.lose(1));
    return child;
  };
  return { spawn, runs };
}

test('the uri list is file URIs, escaped, one a line', () => {
  assert.equal(
    uriList(['/data/chrome/Downloads/a b#?%.png', '/data/chrome/Downloads/café.txt']),
    'file:///data/chrome/Downloads/a%20b%23%3F%25.png\r\nfile:///data/chrome/Downloads/caf%C3%A9.txt\r\n',
  );
});

test('one xclip owns the clipboard per copy, offering only text/uri-list', async () => {
  const x = fakeXclip();
  const clip = createClipboard({ spawn: x.spawn, settleMs: 5 });
  await clip.set(['/d/a.png', '/d/b.txt']);
  assert.equal(x.runs.length, 1);
  assert.equal(x.runs[0].cmd, 'xclip');
  assert.deepEqual(x.runs[0].args, ['-quiet', '-selection', 'clipboard', '-t', 'text/uri-list']);
  assert.equal(x.runs[0].input, 'file:///d/a.png\r\nfile:///d/b.txt\r\n');
  assert.deepEqual(clip.offered(), ['/d/a.png', '/d/b.txt']);

  // The next copy replaces it: the last one wins.
  await clip.set(['/d/c.txt']);
  assert.equal(x.runs[0].killed, true);
  assert.equal(x.runs[1].killed, false);
  assert.deepEqual(clip.offered(), ['/d/c.txt']);
  await new Promise(setImmediate); // the old one's exit is not the new one's
  assert.deepEqual(clip.offered(), ['/d/c.txt']);
});

test('something else being copied ends it; nothing is offered then', async () => {
  const x = fakeXclip();
  const clip = createClipboard({ spawn: x.spawn, settleMs: 5 });
  await clip.set(['/d/a.png']);
  x.runs[0].lose(); // text sent from the session page, or a copy in Chromium
  assert.deepEqual(clip.offered(), []);
  clip.forget('/d/a.png');
  assert.equal(x.runs[0].killed, false);
});

test('deleting a file on the clipboard gives the clipboard up; another file does not', async () => {
  const x = fakeXclip();
  const clip = createClipboard({ spawn: x.spawn, settleMs: 5 });
  await clip.set(['/d/a.png', '/d/b.png']);
  clip.forget('/d/other.png');
  assert.equal(x.runs[0].killed, false);
  clip.forget('/d/b.png');
  assert.equal(x.runs[0].killed, true);
  assert.deepEqual(clip.offered(), []);
});

test('no xclip, or no display, is reported and leaves nothing offered', async () => {
  for (const failWith of ['enoent', 'exit']) {
    const x = fakeXclip({ failWith });
    const clip = createClipboard({ spawn: x.spawn, settleMs: 50 });
    await assert.rejects(clip.set(['/d/a.png']), ClipboardUnavailable);
    assert.deepEqual(clip.offered(), []);
  }
  const clip = createClipboard({ spawn: () => { throw new Error('EAGAIN'); } });
  await assert.rejects(clip.set(['/d/a.png']), ClipboardUnavailable);
});

// The route.
let root, dir, server, port, sets, forgotten, fail;
before(async () => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), 'clipboard-test-'));
  dir = path.join(root, 'Downloads');
  fs.mkdirSync(dir);
  fs.writeFileSync(path.join(root, 'secret.txt'), 'outside');
  for (const name of ['a.png', 'b c.txt']) fs.writeFileSync(path.join(dir, name), 'x');
  fs.symlinkSync(path.join(root, 'secret.txt'), path.join(dir, 'link.txt'));
  fs.mkdirSync(path.join(dir, 'folder'));
  sets = [];
  forgotten = [];
  const clipboard = {
    set: async (paths) => {
      if (fail) throw new ClipboardUnavailable('no display');
      sets.push(paths);
    },
    forget: (p) => forgotten.push(p),
  };
  const files = createFiles({ dir, clipboard });
  const plain = createFiles({ dir }); // a server with no clipboard
  server = http.createServer(async (req, res) => {
    const handler = req.headers['x-plain'] ? plain : files;
    if (!(await handler(req, res))) res.writeHead(404).end('not files');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  port = server.address().port;
});
after(() => {
  server.close();
  fs.rmSync(root, { recursive: true, force: true });
});

function call(method, rawPath, body, headers = { 'Content-Type': 'application/json' }) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method, path: rawPath, headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString() }));
    });
    req.on('error', reject);
    req.end(body === undefined || typeof body === 'string' ? body : JSON.stringify(body));
  });
}

test('POST /clipboard puts files of the folder on the clipboard, as one selection', async () => {
  const res = await call('POST', '/clipboard', { files: ['a.png', 'b c.txt', 'a.png'] });
  assert.equal(res.status, 204);
  assert.deepEqual(sets, [[path.join(dir, 'a.png'), path.join(dir, 'b c.txt')]]);
});

test('nothing but a file of the folder goes on the clipboard', async () => {
  sets.length = 0;
  for (const name of ['../secret.txt', '..', '/etc/passwd', path.join(root, 'secret.txt'), 'a/b', 'a\\b', '.hidden', 'a\u0000b', '', 7, null, ['a.png']]) {
    const res = await call('POST', '/clipboard', { files: ['a.png', name] });
    assert.equal(res.status, 400, JSON.stringify(name));
  }
  for (const body of [{}, { files: [] }, { files: 'a.png' }, [], 'not json', { files: Array(101).fill('a.png') }, { files: ['x'.repeat(70000)] }]) {
    assert.equal((await call('POST', '/clipboard', body)).status, 400, JSON.stringify(body).slice(0, 40));
  }
  // Valid names that are not files there: gone, a link, a folder.
  for (const name of ['gone.txt', 'link.txt', 'folder']) {
    assert.equal((await call('POST', '/clipboard', { files: ['a.png', name] })).status, 410, name);
  }
  assert.deepEqual(sets, []);
});

test('a page in the session\'s own browser cannot set the clipboard', async () => {
  sets.length = 0;
  const json = { 'Content-Type': 'application/json' };
  for (const headers of [
    { 'Content-Type': 'text/plain' }, // what a page can send without a preflight
    { 'Content-Type': 'application/x-www-form-urlencoded' },
    {},
    { ...json, Origin: 'https://evil.example' },
    { ...json, Origin: 'null' },
    { ...json, 'Sec-Fetch-Site': 'cross-site' },
    { ...json, 'Sec-Fetch-Mode': 'no-cors' },
    { ...json, Host: 'evil.example:8081' },
  ]) {
    const res = await call('POST', '/clipboard', { files: ['a.png'] }, headers);
    assert.equal(res.status, 403, JSON.stringify(headers));
  }
  for (const method of ['GET', 'PUT', 'DELETE']) assert.equal((await call(method, '/clipboard')).status, 405);
  assert.deepEqual(sets, []);
  // Nothing else is under /clipboard, and a server without one has no route.
  assert.equal((await call('POST', '/clipboard/x', { files: ['a.png'] })).body, 'not files');
  assert.equal((await call('POST', '/clipboard', { files: ['a.png'] }, { ...json, 'X-Plain': '1' })).body, 'not files');
});

test('an unavailable clipboard is a 503, and deleting a file tells the clipboard', async () => {
  fail = true;
  assert.equal((await call('POST', '/clipboard', { files: ['a.png'] })).status, 503);
  fail = false;
  assert.equal((await call('DELETE', '/files/a.png', undefined, {})).status, 204);
  assert.deepEqual(forgotten, [path.join(dir, 'a.png')]);
});
