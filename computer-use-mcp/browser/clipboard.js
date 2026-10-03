/**
 * Files on the session's clipboard, so that Ctrl+V in Chromium pastes them.
 *
 * Chromium on X11 reads pasted files from the CLIPBOARD selection's
 * text/uri-list target: file:// URIs, one a line (ClipboardOzone::ReadFilenames
 * in ui/base/clipboard/clipboard_ozone.cc; blink's DataObject::CreateFromClipboard
 * makes each a file of the paste). An X selection is not stored
 * anywhere: whoever owns it answers each paste, for as long as it owns it.
 * That owner is one xclip process per copy. It leaves by itself when
 * something else is copied (in Chromium, or text sent from the session page
 * through the VNC server), and is stopped here when a file it offers is
 * deleted.
 *
 * Only that one target is offered. No text target: Xvnc tells a viewer about
 * a new clipboard owner only if it offers STRING or UTF8_STRING (vncSelection.c),
 * so the session page's Clipboard box is left as it is. No image target: blink
 * makes a file of every image/png on the clipboard besides the files, so an
 * image would arrive in a page twice.
 */

import { spawn as spawnProcess } from 'node:child_process';
import { pathToFileURL } from 'node:url';

export const MAX_CLIPBOARD_FILES = 100;

// How long xclip gets to fail (no display, no such program) before it is
// taken to own the selection.
const SETTLE_MS = 200;

// The text/uri-list (RFC 2483) of absolute paths.
export function uriList(paths) {
  return paths.map((p) => pathToFileURL(p).href + '\r\n').join('');
}

export class ClipboardUnavailable extends Error {}

export function createClipboard({ spawn = spawnProcess, settleMs = SETTLE_MS } = {}) {
  let owner = null; // the xclip that owns the selection, if one of ours does
  let offered = []; // the paths it offers

  function drop() {
    const child = owner;
    owner = null;
    offered = [];
    child?.kill();
  }

  // Puts the files at paths on the clipboard, replacing what was there.
  function set(paths) {
    return new Promise((resolve, reject) => {
      const previous = owner;
      let child;
      try {
        child = spawn('xclip', ['-quiet', '-selection', 'clipboard', '-t', 'text/uri-list'], {
          stdio: ['pipe', 'ignore', 'ignore'],
        });
      } catch (err) {
        return reject(new ClipboardUnavailable(String(err.message || err)));
      }
      let settled = false;
      const gone = (why) => {
        if (owner === child) {
          owner = null;
          offered = [];
        }
        if (!settled) {
          settled = true;
          reject(new ClipboardUnavailable(why));
        }
      };
      child.on('error', (err) => gone(String(err.message || err)));
      // Also how it ends when something else is copied: then nothing is ours.
      child.on('exit', (code) => gone(`xclip exited (${code})`));
      child.stdin.on('error', () => {});
      child.stdin.end(uriList(paths));
      owner = child;
      offered = [...paths];
      // The new owner has taken the selection from the old one, which leaves
      // by itself; make sure of it.
      previous?.kill();
      setTimeout(() => {
        if (settled) return;
        settled = true;
        resolve();
      }, settleMs).unref?.();
    });
  }

  return {
    set,
    drop,
    // A file that is deleted must not stay on the clipboard as a dead path.
    forget(path) {
      if (offered.includes(path)) drop();
    },
    offered: () => [...offered],
  };
}
