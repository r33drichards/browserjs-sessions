// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"
import { LifecycleActions, stateLabel } from "./SessionLifecycle"

afterEach(cleanup)

const run = async () => {}
const session = (extra: object) => ({ id: "s-aaaaa", name: "research", state: "stopping" as const, ...extra })
const only = () => {
  const buttons = screen.getAllByRole("button")
  expect(buttons).toHaveLength(1)
  return buttons[0]
}

describe("a session on its way down", () => {
  it("going to sleep: a Wake that waits until it is asleep, and no Start", () => {
    const s = session({ stateSaved: true })
    expect(stateLabel(s)).toBe("going to sleep")
    render(<LifecycleActions session={s} blocked={null} run={run} />)
    expect(only().textContent).toBe("WakeIt can wake once it is asleep") // the label, then the reason it is disabled
    expect(only().getAttribute("aria-disabled")).toBe("true")
    expect(screen.queryByRole("button", { name: "Start" })).toBeNull()
  })

  it("going to sleep for its account (billing says why): the same", () => {
    render(<LifecycleActions session={session({ stoppedBy: "idle" })} blocked={null} run={run} />)
    expect(only().textContent).toBe("WakeIt can wake once it is asleep") // the label, then the reason it is disabled
    expect(only().getAttribute("aria-disabled")).toBe("true")
  })

  it("being stopped: a Start that waits until it has stopped", () => {
    const s = session({ stoppedBy: "user" })
    expect(stateLabel(s)).toBe("stopping")
    render(<LifecycleActions session={s} blocked={null} run={run} />)
    expect(only().textContent).toBe("StartIt can start once it has stopped")
    expect(only().getAttribute("aria-disabled")).toBe("true")
  })

  it("once down: Wake for one asleep, Start only for one stopped, both ready", () => {
    const { unmount } = render(<LifecycleActions session={session({ state: "asleep" })} blocked={null} run={run} />)
    expect(only().textContent).toBe("Wake")
    expect(only().getAttribute("aria-disabled")).toBeNull()
    unmount()
    render(<LifecycleActions session={session({ state: "stopped" })} blocked={null} run={run} />)
    expect(only().textContent).toBe("Start")
    expect(only().getAttribute("aria-disabled")).toBeNull()
  })
})
