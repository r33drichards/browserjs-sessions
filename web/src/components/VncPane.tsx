import RFB from "@novnc/novnc"
import { useEffect, useRef, useState } from "react"
import { api } from "../shell"
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
        const ticket = await api.vncTicket(sessionId)
        const target = live()
        if (!target) return
        const proto = window.location.protocol === "https:" ? "wss:" : "ws:"
        const url = `${proto}//${window.location.host}/s/${encodeURIComponent(sessionId)}/vnc?ticket=${encodeURIComponent(ticket)}`
        const conn = new RFB(target, url, {})
        rfb = conn
        conn.scaleViewport = true
        conn.resizeSession = false
        conn.addEventListener("connect", () => {
          if (rfb !== conn) return
          attempt = 0
          setStatus("connected")
        })
        conn.addEventListener("disconnect", () => {
          if (rfb !== conn) return // already replaced, paused or unmounted
          rfb = null
          scheduleRetry()
        })
      } catch {
        if (mine === generation) scheduleRetry()
      }
    }

    function drop() {
      generation++
      clearTimeout(retry)
      const conn = rfb
      rfb = null
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

  return (
    <div>
      <div ref={screenRef} className="wf-screen" />
      {status !== "connected" && <p>{STATUS_TEXT[status]}</p>}
    </div>
  )
}
