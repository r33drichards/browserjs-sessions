declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, url: string, options?: Record<string, unknown>)
    scaleViewport: boolean
    resizeSession: boolean
    viewOnly: boolean
    background: string
    disconnect(): void
    focus(options?: FocusOptions): void
    clipboardPasteFrom(text: string): void
    // Internal to noVNC, not part of its API: see ResizingRFB.
    protected _requestRemoteResize(): void
    protected _screenSize(): { w: number; h: number }
  }
}
