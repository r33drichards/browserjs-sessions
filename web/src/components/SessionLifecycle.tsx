// Sleep, wake and stop: the same buttons on the list and on a session's page.
import Button from "@cloudscape-design/components/button"
import ButtonDropdown from "@cloudscape-design/components/button-dropdown"
import { useState } from "react"
import type { Session } from "../api"
import type { WakeBlock } from "../billingApi"
import { WAKE_BLOCK_LABEL } from "../billingApi"
import { api } from "../shell"

type Lifecycle = Pick<Session, "state" | "stoppedBy" | "stateSaved">

// Suspended and waiting for its pod to go, after a sleep rather than a stop.
// Without billing the backend does not say why; a saved state does.
const goingToSleep = (s: Lifecycle) =>
  s.state === "stopping" && (!!s.stateSaved || (!!s.stoppedBy && s.stoppedBy !== "user" && s.stoppedBy !== "blocked"))

// The one label for a session's state. For a session that is down it says
// what starting it again brings back.
export function stateLabel(s: Lifecycle): string {
  if (goingToSleep(s)) return "going to sleep"
  if (s.state === "asleep") return s.stateSaved ? "asleep" : "asleep: state not saved"
  // A session stopped for its account keeps its snapshot.
  if (s.state === "stopped") return s.stateSaved ? "stopped" : "stopped: starts fresh"
  return s.state
}

// The same, in a sentence, for the place of the screen.
export function stateSentence(s: Lifecycle): string {
  if (goingToSleep(s)) return "Going to sleep…"
  switch (s.state) {
    case "starting":
      return "Starting the browser…"
    case "stopping":
      return "Stopping…"
    case "asleep":
      return s.stateSaved
        ? "Asleep, with its state saved. It wakes as it was when you or an agent uses it."
        : "Asleep. Its state was not saved: it starts fresh, with its disk, when you or an agent uses it."
    case "stopped":
      return "Stopped. Its state was not saved: it starts fresh, with its disk, when you start it. An agent's call does not start it."
    case "failed":
      return "The session failed to start."
    default:
      return ""
  }
}

const STOP = "stop"

// Sleep for a session that is awake, Wake (Start, for one that was stopped)
// for one that is not, and Stop beside either as the way to a fresh pod.
// `run` performs the request and reports failure itself; it resolves either way.
export function LifecycleActions({
  session,
  blocked,
  run,
  primary = false,
}: {
  session: Pick<Session, "id" | "name"> & Lifecycle
  blocked: WakeBlock | null // billing keeps it asleep
  run: (request: () => Promise<unknown>) => Promise<unknown>
  primary?: boolean // whether Wake is the page's primary button
}) {
  const [busy, setBusy] = useState<"sleep" | "wake" | "stop" | null>(null)

  async function act(what: "sleep" | "wake" | "stop") {
    setBusy(what)
    try {
      await run(() =>
        what === "sleep" ? api.sleepSession(session.id) : what === "wake" ? api.wakeSession(session.id) : api.setRunning(session.id, false),
      )
    } finally {
      setBusy(null)
    }
  }

  const stop = (text: string, description: string) => ({
    items: [{ id: STOP, text, description }],
    onItemClick: (e: { detail: { id: string } }) => {
      if (e.detail.id === STOP) act("stop")
    },
    ariaLabel: `More actions for ${session.name}`,
    expandToViewport: true,
  })

  if (session.state === "running" || session.state === "starting") {
    const starting = session.state === "starting"
    return (
      <ButtonDropdown
        {...stop("Stop without saving state", "Removes the pod and keeps the disk. It starts fresh, and only when you start it.")}
        disabled={busy !== null}
        mainAction={{
          // The snapshot takes seconds; the request answers when it is done.
          text: busy === "sleep" ? "Saving state…" : "Sleep",
          loading: busy === "sleep",
          loadingText: "Saving state",
          disabled: starting || busy !== null,
          disabledReason: starting ? "It can sleep once it is running" : undefined,
          onClick: () => act("sleep"),
        }}
      />
    )
  }

  const wakeText = session.state === "asleep" ? "Wake" : "Start"
  if (blocked) {
    return (
      <Button variant={primary ? "primary" : "normal"} disabled disabledReason={WAKE_BLOCK_LABEL[blocked]}>
        {wakeText}
      </Button>
    )
  }
  if (session.state === "asleep" && session.stateSaved) {
    return (
      <ButtonDropdown
        {...stop("Stop and discard saved state", "The next start is fresh, with the disk only, and only when you start it.")}
        variant={primary ? "primary" : "normal"}
        disabled={busy !== null}
        mainAction={{ text: wakeText, loading: busy === "wake", disabled: busy !== null, onClick: () => act("wake") }}
      />
    )
  }
  return (
    <Button variant={primary ? "primary" : "normal"} loading={busy === "wake"} onClick={() => act("wake")}>
      {wakeText}
    </Button>
  )
}
