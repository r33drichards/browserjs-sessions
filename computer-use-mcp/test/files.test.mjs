// Usage: node --test computer-use-mcp/test/files.test.mjs  (needs only node)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { after, before, test } from 'node:test';
import { createFiles, setDownloadDir, validName } from '../browser/files.js';

const MAX = 1024;
let root, dir, server, port;

before(async () => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), 'files-test-'));
  dir = path.join(root, 'Downloads');
  fs.writeFileSync(path.join(root, 'secret.txt'), 'outside the folder');
  fs.mkdirSync(dir);
  fs.writeFileSync(path.join(dir, '.upload-left-over'), 'half a file');
  const files = createFiles({ dir, maxBytes: MAX });
  server = http.createServer(async (req, res) => {
    if (!(await files(req, res))) res.writeHead(404).end('not files');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  port = server.address().port;
});

after(() => {
  server.close();
  fs.rmSync(root, { recursive: true, force: true });
});

// A request with the path sent exactly as written (fetch would tidy "..").
function call(method, rawPath, { body, headers = {} } = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method, path: rawPath, headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString() }));
    });
    req.on('error', reject);
    req.end(body);
  });
}
const names = async () => JSON.parse((await call('GET', '/files')).body).map((f) => f.name);

test('an upload is listed, downloads as an attachment, and deletes', async () => {
  const put = await call('PUT', '/files/hello%20world.txt', { body: 'hello' });
  assert.equal(put.status, 201);
  assert.deepEqual(JSON.parse(put.body), { name: 'hello world.txt', size: 5 });
  assert.equal(fs.readFileSync(path.join(dir, 'hello world.txt'), 'utf8'), 'hello');

  const list = await call('GET', '/files');
  assert.equal(list.headers['content-type'], 'application/json');
  const [entry] = JSON.parse(list.body);
  assert.equal(entry.name, 'hello world.txt');
  assert.equal(entry.size, 5);
  assert.ok(!Number.isNaN(Date.parse(entry.modified)));

  const get = await call('GET', '/files/hello%20world.txt');
  assert.equal(get.status, 200);
  assert.equal(get.body, 'hello');
  assert.equal(get.headers['content-type'], 'application/octet-stream');
  assert.equal(get.headers['content-disposition'], 'attachment');
  assert.equal(get.headers['content-length'], '5');
  assert.equal(get.headers['x-content-type-options'], 'nosniff');

  assert.equal((await call('DELETE', '/files/hello%20world.txt')).status, 204);
  assert.equal((await call('GET', '/files/hello%20world.txt')).status, 404);
  assert.equal((await call('DELETE', '/files/hello%20world.txt')).status, 404);
  assert.deepEqual(await names(), []);
});

test('an upload never replaces a file', async () => {
  for (const want of ['a.tar.gz', 'a.tar (1).gz', 'a.tar (2).gz']) {
    const put = await call('PUT', '/files/a.tar.gz', { body: want });
    assert.equal(JSON.parse(put.body).name, want);
  }
  assert.equal(fs.readFileSync(path.join(dir, 'a.tar.gz'), 'utf8'), 'a.tar.gz');
  assert.equal(JSON.parse((await call('PUT', '/files/noext', { body: '1' })).body).name, 'noext');
  assert.equal(JSON.parse((await call('PUT', '/files/noext', { body: '2' })).body).name, 'noext (1)');
});

test('names cannot leave the folder', async () => {
  for (const name of [
    '..', '.', '%2e%2e', '..%2Fsecret.txt', '%2E%2E%2Fsecret.txt', '..%5Csecret.txt', 'a%2Fb', 'a/b', '../secret.txt',
    'a/../../secret.txt', '.hidden', '.upload-left-over', 'a%00b', 'a%0Ab', '%', '%FF', 'x'.repeat(256),
  ]) {
    for (const method of ['GET', 'PUT', 'DELETE']) {
      const res = await call(method, '/files/' + name, { body: method === 'PUT' ? 'evil' : undefined });
      assert.ok(res.status >= 400 && res.status < 500, `${method} ${name}: ${res.status}`);
      assert.notEqual(res.body, 'outside the folder');
    }
  }
  assert.equal(fs.readFileSync(path.join(root, 'secret.txt'), 'utf8'), 'outside the folder');
  assert.deepEqual(fs.readdirSync(root).sort(), ['Downloads', 'secret.txt']);
  for (const [name, ok] of [['a.txt', true], ['é (1).pdf', true], ['', false], ['a/b', false], ['a\\b', false], ['.a', false], [undefined, false]]) {
    assert.equal(validName(name), ok, String(name));
  }
});

test('links, folders and unfinished files are neither listed nor served', async () => {
  fs.symlinkSync(path.join(root, 'secret.txt'), path.join(dir, 'link.txt'));
  fs.mkdirSync(path.join(dir, 'folder'));
  fs.writeFileSync(path.join(dir, 'movie.mp4.crdownload'), 'partial');
  const listed = await names();
  for (const name of ['link.txt', 'folder', 'movie.mp4.crdownload', '.upload-left-over']) assert.ok(!listed.includes(name), name);
  for (const name of ['link.txt', 'folder']) {
    assert.equal((await call('GET', '/files/' + name)).status, 404);
    assert.equal((await call('DELETE', '/files/' + name)).status, 404);
  }
  assert.ok(fs.existsSync(path.join(root, 'secret.txt')));
  // Left by a process that died mid-upload: cleared when the next one starts.
  assert.ok(!fs.existsSync(path.join(dir, '.upload-left-over')));
});

test('a file over the limit is refused and leaves nothing behind', async () => {
  const count = fs.readdirSync(dir).length;
  // Declared.
  assert.equal((await call('PUT', '/files/big.bin', { body: 'x'.repeat(MAX + 1) })).status, 413);
  // Not declared (chunked).
  const chunked = await call('PUT', '/files/big.bin', { body: 'x'.repeat(MAX * 4), headers: { 'Transfer-Encoding': 'chunked' } });
  assert.equal(chunked.status, 413);
  assert.equal(fs.readdirSync(dir).length, count);
  // At the limit.
  assert.equal((await call('PUT', '/files/big.bin', { body: 'x'.repeat(MAX) })).status, 201);
  assert.equal(fs.statSync(path.join(dir, 'big.bin')).size, MAX);
});

test('an upload the caller abandons leaves nothing behind', async () => {
  const count = fs.readdirSync(dir).length;
  await new Promise((resolve) => {
    const req = http.request({ host: '127.0.0.1', port, method: 'PUT', path: '/files/half.bin', headers: { 'Content-Length': 100 } });
    req.on('error', () => {});
    req.write('only some', () => setTimeout(() => (req.destroy(), resolve()), 100));
  });
  for (let i = 0; i < 50 && fs.readdirSync(dir).length !== count; i++) await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(fs.readdirSync(dir).filter((n) => n.startsWith('.upload-') || n === 'half.bin'), []);
});

test('only the pod\'s own host names are served, and only the methods there are', async () => {
  for (const host of ['evil.example', 'evil.example:8081', '10.0.0.7:8081']) {
    assert.equal((await call('GET', '/files', { headers: { Host: host } })).status, 403);
  }
  for (const host of ['localhost:8081', '127.0.0.1:8081', 'localhost']) {
    assert.equal((await call('GET', '/files', { headers: { Host: host } })).status, 200);
  }
  assert.equal((await call('POST', '/files/a.txt', { body: 'x' })).status, 405);
  assert.equal((await call('DELETE', '/files')).status, 405);
  // Everything else on the port is somebody else's.
  for (const p of ['/mcp', '/healthz', '/filesystem', '/']) assert.equal((await call('GET', p)).body, 'not files');
});

test('Chromium is pointed at the folder, keeping the preferences it has', () => {
  const profile = path.join(root, 'profile');
  const downloads = path.join(profile, 'Downloads');
  const prefs = () => JSON.parse(fs.readFileSync(path.join(profile, 'Default', 'Preferences'), 'utf8'));
  try {
    // A new profile has no preferences yet.
    setDownloadDir(profile, downloads);
    assert.ok(fs.statSync(downloads).isDirectory());
    assert.equal(prefs().download.default_directory, downloads);
    assert.equal(prefs().download.prompt_for_download, false);
    assert.equal(prefs().savefile.default_directory, downloads);
    assert.equal(prefs().selectfile.last_directory, downloads);

    // One that does keeps them; only where files go is put back.
    fs.writeFileSync(
      path.join(profile, 'Default', 'Preferences'),
      JSON.stringify({ profile: { exit_type: 'Normal' }, download: { default_directory: '/tmp', other: 1 }, selectfile: { last_directory: '/etc' } }),
    );
    setDownloadDir(profile, downloads);
    assert.deepEqual(prefs().profile, { exit_type: 'Normal' });
    assert.equal(prefs().download.other, 1);
    assert.equal(prefs().download.default_directory, downloads);
    assert.equal(prefs().selectfile.last_directory, downloads);

    // Preferences it cannot read are left exactly as they are.
    fs.writeFileSync(path.join(profile, 'Default', 'Preferences'), '{"trunc');
    assert.throws(() => setDownloadDir(profile, downloads));
    assert.equal(fs.readFileSync(path.join(profile, 'Default', 'Preferences'), 'utf8'), '{"trunc');
  } finally {
    fs.rmSync(profile, { recursive: true, force: true });
  }
});
