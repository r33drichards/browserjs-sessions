/**
 * The session's files: one flat folder, which Chromium downloads to and its
 * file chooser opens in, served over HTTP so the session page can put files
 * in and take them out.
 *
 *   GET    /files         the list: [{ name, size, modified }], newest first
 *   GET    /files/<name>  the file, as an attachment
 *   PUT    /files/<name>  store the body; never replaces: answers the name used
 *   DELETE /files/<name>
 *
 * There is no login here. The port is reachable by the backend, which checks
 * who is asking, and from inside the pod.
 */

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { pipeline } from 'node:stream/promises';

export const DEFAULT_MAX_BYTES = 100 * 1024 * 1024;

const UPLOAD_PREFIX = '.upload-';
const MAX_LISTED = 1000;

// One file of the folder and nothing else: a single path segment that is not
// hidden (which also keeps "." and "..", and uploads in progress, out).
export function validName(name) {
  return (
    typeof name === 'string' &&
    name.length > 0 &&
    Buffer.byteLength(name) <= 255 &&
    !name.startsWith('.') &&
    // eslint-disable-next-line no-control-regex
    !/[/\\\u0000-\u001f\u007f]/.test(name)
  );
}

// "a.txt" -> "a (1).txt", as Chromium names a download that already exists.
function numbered(name, n) {
  if (n === 0) return name;
  const ext = path.extname(name);
  return `${name.slice(0, name.length - ext.length)} (${n})${ext}`;
}

class TooLarge extends Error {}
class HungUp extends Error {}

function send(res, status, body, headers = {}) {
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  res.writeHead(status, {
    'Content-Type': typeof body === 'string' ? 'text/plain; charset=utf-8' : 'application/json',
    'Content-Length': Buffer.byteLength(text),
    'Cache-Control': 'no-store',
    'X-Content-Type-Options': 'nosniff',
    ...headers,
  });
  res.end(text);
}

// A page open in this very browser can reach this port too. Its requests
// cannot be read by it (no CORS here) unless it points a host name of its
// own at 127.0.0.1, so only the names the pod is really called by are served.
function localHost(req) {
  const host = String(req.headers.host || '').replace(/:\d+$/, '');
  return host === 'localhost' || host === '127.0.0.1' || host === '[::1]';
}

export function createFiles({ dir, maxBytes = DEFAULT_MAX_BYTES }) {
  fs.mkdirSync(dir, { recursive: true });
  // Uploads a previous process did not finish.
  for (const name of fs.readdirSync(dir)) {
    if (name.startsWith(UPLOAD_PREFIX)) fs.rmSync(path.join(dir, name), { force: true });
  }

  async function list(res) {
    const files = [];
    for (const entry of await fs.promises.readdir(dir, { withFileTypes: true })) {
      // Not folders or links, and not a download Chromium is still writing.
      if (!entry.isFile() || !validName(entry.name) || entry.name.endsWith('.crdownload')) continue;
      try {
        const stat = await fs.promises.lstat(path.join(dir, entry.name));
        files.push({ name: entry.name, size: stat.size, modified: stat.mtime.toISOString() });
      } catch {} // gone since it was listed
    }
    files.sort((a, b) => (a.modified < b.modified ? 1 : a.modified > b.modified ? -1 : a.name.localeCompare(b.name)));
    send(res, 200, files.slice(0, MAX_LISTED));
  }

  async function get(res, name) {
    let handle;
    try {
      // Never through a link: a page can make Chromium write what it likes here.
      handle = await fs.promises.open(path.join(dir, name), fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
      const stat = await handle.stat();
      if (!stat.isFile()) throw new Error('not a file');
      res.writeHead(200, {
        'Content-Type': 'application/octet-stream',
        'Content-Length': stat.size,
        'Content-Disposition': 'attachment',
        'Cache-Control': 'no-store',
        'X-Content-Type-Options': 'nosniff',
      });
      // Exactly the size announced, even if the file grows meanwhile.
      const stream = stat.size > 0 ? handle.createReadStream({ start: 0, end: stat.size - 1 }) : null;
      if (!stream) return res.end();
      handle = null; // the stream closes it
      await pipeline(stream, res);
    } catch (err) {
      if (res.headersSent) res.destroy();
      else send(res, 404, 'no such file');
    } finally {
      await handle?.close().catch(() => {});
    }
  }

  async function put(req, res, name) {
    // The body is not read: say so, or the connection would be reused mid-body.
    const refuse = (status, text) => send(res, status, text, { Connection: 'close' });
    if (Number(req.headers['content-length']) > maxBytes) return refuse(413, 'file too large');

    const tmp = path.join(dir, UPLOAD_PREFIX + crypto.randomUUID());
    // Straight to disk under a hidden name; it takes its own name only once
    // it is whole. Not a pipeline: that would hang up on a caller who is
    // still owed an answer.
    const out = fs.createWriteStream(tmp, { flags: 'wx', mode: 0o644 });
    let size = 0;
    try {
      await new Promise((resolve, reject) => {
        out.on('error', reject);
        req.on('error', reject);
        req.on('close', () => req.complete || reject(new HungUp()));
        req.on('end', () => out.end(resolve));
        req.on('data', (chunk) => {
          size += chunk.length;
          if (size > maxBytes) return reject(new TooLarge());
          if (!out.write(chunk)) {
            req.pause();
            out.once('drain', () => req.resume());
          }
        });
      });
    } catch (err) {
      req.removeAllListeners('data').on('error', () => {}).resume(); // the rest is discarded
      out.destroy();
      fs.rmSync(tmp, { force: true });
      if (err instanceof HungUp) return res.destroy();
      if (err instanceof TooLarge) return refuse(413, 'file too large');
      if (err.code === 'ENOSPC' || err.code === 'EDQUOT') return refuse(507, 'the session disk is full');
      return refuse(500, 'could not store the file');
    }
    try {
      for (let n = 0; n < 1000; n++) {
        const final = numbered(name, n);
        if (!validName(final)) break; // the number made it too long
        try {
          fs.linkSync(tmp, path.join(dir, final)); // fails rather than replace
        } catch (err) {
          if (err.code === 'EEXIST') continue;
          throw err;
        }
        return send(res, 201, { name: final, size });
      }
      send(res, 409, 'a file of that name already exists');
    } catch {
      send(res, 500, 'could not store the file');
    } finally {
      fs.rmSync(tmp, { force: true });
    }
  }

  async function remove(res, name) {
    try {
      const file = path.join(dir, name);
      if (!(await fs.promises.lstat(file)).isFile()) throw new Error('not a file');
      await fs.promises.unlink(file);
      res.writeHead(204).end();
    } catch {
      send(res, 404, 'no such file');
    }
  }

  // Answers the request if it is for /files, and says whether it did.
  return async function handle(req, res) {
    const raw = String(req.url).split('?')[0];
    if (raw !== '/files' && !raw.startsWith('/files/')) return false;
    try {
      if (!localHost(req)) return send(res, 403, 'forbidden'), true;
      if (raw === '/files' || raw === '/files/') {
        if (req.method !== 'GET') return send(res, 405, 'method not allowed', { Allow: 'GET' }), true;
        await list(res);
        return true;
      }
      let name;
      try {
        // Decoded once, as one segment: an encoded separator is then a
        // separator, and refused.
        name = decodeURIComponent(raw.slice('/files/'.length));
      } catch {}
      if (!validName(name)) return send(res, 400, 'not a file name'), true;
      if (req.method === 'GET') await get(res, name);
      else if (req.method === 'PUT') await put(req, res, name);
      else if (req.method === 'DELETE') await remove(res, name);
      else send(res, 405, 'method not allowed', { Allow: 'GET, PUT, DELETE' });
    } catch (err) {
      if (res.headersSent) res.destroy();
      else send(res, 500, 'the files could not be read');
    }
    return true;
  };
}

/**
 * Point Chromium at the folder: downloads go there without asking, and the
 * file chooser (attach, save as) opens there. Run before Chromium starts,
 * which reads its preferences once and writes them back when it quits; a
 * profile that has none yet gets just these.
 */
export function setDownloadDir(profileDir, dir) {
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(profileDir, 'Default', 'Preferences');
  let prefs = {};
  try {
    prefs = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    // Unreadable preferences are Chromium's to deal with, not ours to replace.
    if (err.code !== 'ENOENT') throw err;
  }
  if (!prefs || typeof prefs !== 'object' || Array.isArray(prefs)) throw new Error(`${file} is not an object`);
  const section = (key) => (prefs[key] && typeof prefs[key] === 'object' ? prefs[key] : (prefs[key] = {}));
  Object.assign(section('download'), { default_directory: dir, directory_upgrade: true, prompt_for_download: false });
  section('savefile').default_directory = dir;
  section('selectfile').last_directory = dir;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  // Rename over the old file so a crash mid-write cannot leave it truncated.
  fs.writeFileSync(file + '.tmp', JSON.stringify(prefs));
  fs.renameSync(file + '.tmp', file);
}
