// The process nut.js runs in (see desktop.js for why it is not the server).
// One message in, one out: { id, size, operations, config } ->
// { id, results, images, screen, released? } or { id, error }.

import { runOperations } from './desktop.js';

let nutPromise = null;

// Loaded on the first call, so a platform the native addon has no build for
// answers with the reason instead of failing to start.
function loadNut() {
  nutPromise ||= import('@nut-tree-fork/nut-js').then((m) => m.default);
  return nutPromise;
}

process.on('message', async (msg) => {
  try {
    let nut;
    try {
      nut = await loadNut();
    } catch (err) {
      nutPromise = null;
      throw new Error(`nut.js could not be loaded (${process.platform}/${process.arch}, DISPLAY=${process.env.DISPLAY || ''}): ${err.message || err}`);
    }
    process.send({ id: msg.id, ...(await runOperations(nut, msg, { size: msg.size || undefined })) });
  } catch (err) {
    process.send({ id: msg.id, error: (err && err.message) || String(err) });
  }
});

// The server went away: nothing left to answer to.
process.on('disconnect', () => process.exit(0));
