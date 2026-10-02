import RFB from "@novnc/novnc"
import { remoteSize, RESIZE_DEBOUNCE_MS } from "./remoteSize"

// noVNC with `resizeSession` asks the server to make the desktop the size of
// the screen box, about ten times a second while the box changes and at any
// size. This waits for the box to settle and keeps the request within limits.
//
// noVNC has no public hook for either, so this overrides two of its internal
// methods (as of 1.7.0). If a later version renames them the overrides are
// simply never called and noVNC resizes the way it does on its own.
//
// A server that cannot resize (an older session image) never offers to, or
// refuses; noVNC then leaves the desktop alone and `scaleViewport` fits the
// picture into the box, as before.
export class ResizingRFB extends RFB {
  private settle: ReturnType<typeof setTimeout> | undefined
  private asking = false
  private gone = false

  constructor(target: HTMLElement, url: string) {
    super(target, url, {})
    this.addEventListener("disconnect", () => this.stop())
  }

  private stop() {
    this.gone = true
    clearTimeout(this.settle)
  }

  disconnect() {
    this.stop()
    super.disconnect()
  }

  protected _requestRemoteResize() {
    clearTimeout(this.settle)
    if (this.gone) return
    this.settle = setTimeout(() => {
      if (this.gone || !remoteSize(...this.box())) return
      this.asking = true
      try {
        super._requestRemoteResize()
      } finally {
        this.asking = false
      }
    }, RESIZE_DEBOUNCE_MS)
  }

  // Also what noVNC scales the picture to, which must stay the real box.
  protected _screenSize() {
    const [w, h] = this.box()
    return (this.asking && remoteSize(w, h)) || { w, h }
  }

  private box(): [number, number] {
    const { w, h } = super._screenSize()
    return [w, h]
  }
}
