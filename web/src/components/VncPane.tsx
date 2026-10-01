import RFB from "@novnc/novnc"
import { useEffect, useRef, useState } from "react"
import { api } from "../shell"

type Status = "connecting" | "connected" | "reconnecting"

export function VncPane({ sessionId }: { sessionId: string }) {
  const screenRef = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>("connecting")

  useEffect(() => {
    let rfb: RFB | null = null
    let stopped = false
    let retry: ReturnType<typeof setTimeout> | undefined

    async function connect() {
      if (stopped || !screenRef.current) return
      try {
        const ticket = await api.vncTicket(sessionId)
        if (stopped || !screenRef.current) return
        const proto = window.location.protocol === "https:" ? "wss:" : "ws:"
        const url = `${proto}//${window.location.host}/s/${sessionId}/vnc?ticket=${ticket}`
        rfb = new RFB(screenRef.current, url, {})
        rfb.scaleViewport = true
        rfb.resizeSession = false
        rfb.addEventListener("connect", () => setStatus("connected"))
        rfb.addEventListener("disconnect", () => {
          rfb = null
          if (stopped) return
          setStatus("reconnecting")
          retry = setTimeout(connect, 2000)
        })
      } catch {
        if (stopped) return
        setStatus("reconnecting")
        retry = setTimeout(connect, 2000)
      }
    }

    connect()
    return () => {
      stopped = true
      clearTimeout(retry)
      rfb?.disconnect()
    }
  }, [sessionId])

  return (
    <div>
      <div ref={screenRef} className="wf-screen" />
      {status !== "connected" && (
        <p>{status === "connecting" ? "Connecting to the browser…" : "Connection lost — reconnecting…"}</p>
      )}
    </div>
  )
}
