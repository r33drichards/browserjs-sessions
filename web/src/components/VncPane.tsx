import Button from "@cloudscape-design/components/button"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Textarea from "@cloudscape-design/components/textarea"
import RFB from "@novnc/novnc"
import { useEffect, useRef, useState } from "react"
import { api } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { reconnectDelay } from "./backoff"

type Status = "connecting" | "connected" | "reconnecting" | "paused"

// An open VNC websocket keeps the session awake, so a tab left in the
// background for this long lets go of it and the session can go to sleep.
const HIDDEN_GRACE_MS = 60_000

const STATUS_TEXT: Record<Exclude<Status, "connected">, string> = {
  connecting: "Connecting to the browser…",
  reconnecting: "Connection lost — reconnecting…",
  paused: "Paused while this tab is in the background",
}

export function VncPane({ sessionId }: { sessionId: string }) {
  const screenRef = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>("connecting")
  // The live connection, for the clipboard box below the screen.
  const rfbRef = useRef<RFB | null>(null)
  const [clip, setClip] = useState("")
  const [note, setNote] = useState("")

  useEffect(() => {
    let rfb: RFB | null = null
    let stopped = false
    let paused = false
    let attempt = 0 // failed connects since the last successful one
    let generation = 0 // bumped to abandon a connect that is still awaiting its ticket
    let retry: ReturnType<typeof setTimeout> | undefined
    let hiddenTimer: ReturnType<typeof setTimeout> | undefined

    function scheduleRetry() {
      if (stopped || paused) return
      setStatus("reconnecting")
      retry = setTimeout(connect, reconnectDelay(attempt++))
    }

    async function connect() {
      const mine = ++generation
      const live = () => !stopped && !paused && mine === generation && screenRef.current
      if (!live()) return
      try {
        // The session lives on its own host; the backend says where.
        const { url } = await api.vncTicket(sessionId)
        const target = live()
        if (!target) return
        const conn = new RFB(target, url, {})
        rfb = conn
        conn.scaleViewport = true
        conn.background = "#fff" // noVNC's own is a dark grey slab
        conn.resizeSession = false
        conn.addEventListener("connect", () => {
          if (rfb !== conn) return
          attempt = 0
          rfbRef.current = conn
          setStatus("connected")
        })
        // Text copied in the remote browser lands in the box.
        conn.addEventListener("clipboard", e => {
          if (rfb !== conn) return
          setClip((e as CustomEvent<{ text: string }>).detail.text)
          setNote("Copied in the browser. Use Copy to put it on your clipboard.")
        })
        conn.addEventListener("disconnect", () => {
          if (rfb !== conn) return // already replaced, paused or unmounted
          rfb = null
          rfbRef.current = null
          scheduleRetry()
        })
      } catch (e) {
        if (signedOutHandled(e)) return // the page is reloading; don't keep retrying
        if (mine === generation) scheduleRetry()
      }
    }

    function drop() {
      generation++
      clearTimeout(retry)
      const conn = rfb
      rfb = null
      rfbRef.current = null
      conn?.disconnect()
    }

    function pause() {
      paused = true
      drop()
      setStatus("paused")
    }

    function onVisibility() {
      if (document.visibilityState === "hidden") {
        if (!paused && hiddenTimer === undefined) hiddenTimer = setTimeout(pause, HIDDEN_GRACE_MS)
        return
      }
      clearTimeout(hiddenTimer)
      hiddenTimer = undefined
      if (!paused) return
      paused = false
      attempt = 0
      setStatus("connecting")
      connect()
    }

    connect()
    onVisibility() // a tab opened in the background starts its grace period now
    document.addEventListener("visibilitychange", onVisibility)
    return () => {
      stopped = true
      clearTimeout(hiddenTimer)
      document.removeEventListener("visibilitychange", onVisibility)
      drop()
    }
  }, [sessionId])

  function send(text: string) {
    const conn = rfbRef.current
    if (!conn) return
    conn.clipboardPasteFrom(text)
    setNote("Sent. Press Ctrl+V in the browser to paste it.")
  }

  // One step instead of two, where this browser lets the page read the clipboard.
  async function sendMine() {
    try {
      const text = await navigator.clipboard.readText()
      setClip(text)
      send(text)
    } catch {
      setNote("Couldn't read your clipboard. Paste into the box and use Send to browser.")
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(clip)
      setNote("Copied to your clipboard.")
    } catch {
      setNote("Couldn't copy. Select the text in the box and copy it yourself.")
    }
  }

  const connected = status === "connected"

  return (
    <div>
      <div className="wf-screen-wrap">
        <div ref={screenRef} className="wf-screen" />
        {!connected && (
          <div className="wf-screen-overlay" role="status">
            {status !== "paused" && <span className="wf-spinner" aria-hidden="true" />}
            <p>{STATUS_TEXT[status]}</p>
          </div>
        )}
      </div>
      <div className="wf-box wf-clipboard">
        <SpaceBetween size="xs">
          <strong>Clipboard</strong>
          <Textarea
            value={clip}
            onChange={e => setClip(e.detail.value)}
            rows={3}
            ariaLabel="Clipboard shared with the browser"
            placeholder="Text copied in the browser shows up here. Type or paste text to send it there."
          />
          <SpaceBetween direction="horizontal" size="xs" alignItems="center">
            <Button disabled={!connected || !clip} onClick={() => send(clip)}>
              Send to browser
            </Button>
            <Button disabled={!connected} onClick={sendMine}>
              Send my clipboard
            </Button>
            <Button disabled={!clip} onClick={copy}>
              Copy
            </Button>
            {note && <span className="wf-note">{note}</span>}
          </SpaceBetween>
        </SpaceBetween>
      </div>
    </div>
  )
}
