// The remote desktop takes the size of the box it is shown in, within limits.
export const MAX_REMOTE_WIDTH = 2560
export const MAX_REMOTE_HEIGHT = 1600
const MIN_REMOTE_WIDTH = 320
const MIN_REMOTE_HEIGHT = 200

// How long the box must hold still before the remote desktop is resized:
// every resize makes the remote browser lay its page out again.
export const RESIZE_DEBOUNCE_MS = 250

// The desktop size to ask for when the screen box is w by h CSS pixels: the
// box itself, or for a box over the limit the largest size of the same shape
// that fits (the picture is then scaled up). Null for a box too small to be a
// desktop (collapsed, or not laid out yet): the remote keeps its size.
export function remoteSize(w: number, h: number): { w: number; h: number } | null {
  if (!(w >= MIN_REMOTE_WIDTH && h >= MIN_REMOTE_HEIGHT)) return null
  const shrink = Math.max(w / MAX_REMOTE_WIDTH, h / MAX_REMOTE_HEIGHT, 1)
  return {
    w: Math.max(MIN_REMOTE_WIDTH, Math.floor(w / shrink)),
    h: Math.max(MIN_REMOTE_HEIGHT, Math.floor(h / shrink)),
  }
}
