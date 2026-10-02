// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { ClipboardReadError, pasteIsForSession, pastedFiles, pastedName, readClipboardFiles } from "../filesInput"
import { FilesBox } from "./FilesBox"

const ID = "s-aaaaaaaaaa"

// Uploads go through XMLHttpRequest; here they are recorded and "stored"
// under the name they were sent with.
const uploaded: { name: string; type: string }[] = []
vi.mock("../files", async original => ({
  ...(await original<typeof import("../files")>()),
  uploadFile: async (_id: string, file: File, name: string) => {
    uploaded.push({ name, type: file.type })
    return name
  },
}))

let copied: string[][]
beforeEach(() => {
  uploaded.length = 0
  copied = []
  globalThis.__testFetch = async (input, init) => {
    if (String(input).endsWith("/clipboard")) {
      copied.push(JSON.parse(String(init?.body)).files)
      return new Response(null, { status: 204 })
    }
    return new Response(JSON.stringify({ files: [], max_bytes: 1000 }), { headers: { "Content-Type": "application/json" } })
  }
})
afterEach(cleanup)

const file = (name: string, type = "text/plain", body = "x") => new File([body], name, { type })

// What a paste or a drag carries (jsdom has no DataTransfer).
function transfer(files: File[], text?: string, viaItems = false) {
  const types = [...(files.length ? ["Files"] : []), ...(text === undefined ? [] : ["text/plain"])]
  return {
    files: viaItems ? [] : files,
    items: files.map(f => ({ kind: "file", type: f.type, getAsFile: () => f })),
    types,
    dropEffect: "none",
  }
}

function paste(target: Element | Document, data: ReturnType<typeof transfer>) {
  const event = new Event("paste", { bubbles: true, cancelable: true })
  Object.defineProperty(event, "clipboardData", { value: data })
  act(() => void target.dispatchEvent(event))
  return event
}

function drag(type: "dragover" | "drop", target: Element, data: ReturnType<typeof transfer>) {
  const event = new Event(type, { bubbles: true, cancelable: true })
  Object.defineProperty(event, "dataTransfer", { value: data })
  act(() => void target.dispatchEvent(event))
  return event
}

// The session page as far as these tests need it: the remote screen's
// canvas, a text field, and the Files box.
function page() {
  const view = render(
    <div>
      <div className="wf-screen">
        <canvas tabIndex={-1} data-testid="canvas" />
      </div>
      <textarea aria-label="Clipboard shared with the browser" />
      <p data-testid="elsewhere">elsewhere on the page</p>
      <FilesBox sessionId={ID} />
    </div>,
  )
  return { ...view, canvas: screen.getByTestId("canvas"), text: screen.getByLabelText("Clipboard shared with the browser") }
}

const names = () => uploaded.map(u => u.name)

describe("pasting files on the session page", () => {
  it("sends pasted files, and puts them on the browser's clipboard together", async () => {
    page()
    const event = paste(document.body, transfer([file("a.pdf"), file("b.txt")]))
    expect(event.defaultPrevented).toBe(true)
    await waitFor(() => expect(copied).toEqual([["a.pdf", "b.txt"]]))
    expect(names()).toEqual(["a.pdf", "b.txt"])
  })

  it("can be pasted into: the box takes the focus and says so", async () => {
    page()
    const box = screen.getByLabelText("Files: click here and paste, or drop files")
    expect(box.tabIndex).toBe(0)
    box.focus()
    expect(document.activeElement).toBe(box)
    expect(box.textContent).toMatch(/click here and paste/)
    paste(box, transfer([file("a.pdf")]))
    await waitFor(() => expect(names()).toEqual(["a.pdf"]))
  })

  it("names a pasted screenshot, which arrives as an item called image.png", async () => {
    page()
    paste(document.body, transfer([file("image.png", "image/png")], undefined, true))
    await waitFor(() => expect(uploaded.length).toBe(1))
    expect(uploaded[0].name).toMatch(/^pasted-\d{4}-\d{2}-\d{2}-\d{4}\.png$/)
    expect(uploaded[0].type).toBe("image/png")
  })

  it("leaves a paste alone while the focus is in the remote screen", async () => {
    const { canvas } = page()
    canvas.focus()
    const onCanvas = paste(canvas, transfer([file("a.pdf")]))
    // And if the event only reaches the document, the focus still says whose it is.
    const onDocument = paste(document, transfer([file("a.pdf")]))
    await new Promise(r => setTimeout(r, 20))
    expect(onCanvas.defaultPrevented).toBe(false)
    expect(onDocument.defaultPrevented).toBe(false)
    expect(names()).toEqual([])
  })

  it("leaves text pastes alone, in a field and anywhere else", async () => {
    const { text } = page()
    text.focus()
    // Plain text; and text that comes with a picture of itself (a spreadsheet).
    const plain = paste(text, transfer([], "hello"))
    const both = paste(text, transfer([file("image.png", "image/png")], "A1\tB1"))
    const outside = paste(document.body, transfer([], "hello"))
    await new Promise(r => setTimeout(r, 20))
    for (const e of [plain, both, outside]) expect(e.defaultPrevented).toBe(false)
    expect(names()).toEqual([])
    // A file and no text is for the session even with a field focused.
    paste(text, transfer([file("a.pdf")]))
    await waitFor(() => expect(names()).toEqual(["a.pdf"]))
  })
})

describe("dropping files on the session page", () => {
  it("takes a drop anywhere, the remote screen included, and keeps the browser from opening the file", async () => {
    const { canvas } = page()
    const elsewhere = screen.getByTestId("elsewhere")
    const below = vi.fn() // what noVNC, or anything else under the page, would see
    canvas.addEventListener("drop", below)

    const over = drag("dragover", elsewhere, transfer([file("a.pdf")]))
    expect(over.defaultPrevented).toBe(true)
    expect(document.querySelector(".wf-drop-overlay")).not.toBeNull()

    const dropped = drag("drop", canvas, transfer([file("a.pdf")]))
    expect(dropped.defaultPrevented).toBe(true)
    expect(below).not.toHaveBeenCalled()
    expect(document.querySelector(".wf-drop-overlay")).toBeNull()
    await waitFor(() => expect(copied).toEqual([["a.pdf"]]))

    drag("drop", elsewhere, transfer([file("b.txt")]))
    await waitFor(() => expect(names()).toEqual(["a.pdf", "b.txt"]))
  })

  it("leaves a drag of text alone, and stops listening when the page is left", async () => {
    const { unmount } = page()
    const text = drag("drop", document.body, transfer([], "hello"))
    expect(text.defaultPrevented).toBe(false)
    unmount()
    const after = drag("drop", document.body, transfer([file("a.pdf")]))
    paste(document.body, transfer([file("a.pdf")]))
    await new Promise(r => setTimeout(r, 20))
    expect(after.defaultPrevented).toBe(false)
    expect(names()).toEqual([])
  })
})

describe("Paste from my clipboard", () => {
  const withClipboard = (clipboard: unknown) => Object.defineProperty(navigator, "clipboard", { value: clipboard, configurable: true })

  it("sends the picture the browser hands over", async () => {
    withClipboard({
      read: async () => [
        { types: ["text/html", "image/png"], getType: async (t: string) => new Blob(["png"], { type: t }) },
        { types: ["text/plain"], getType: async () => new Blob(["x"]) },
      ],
    })
    page()
    fireEvent.click(screen.getByRole("button", { name: "Paste from my clipboard" }))
    await waitFor(() => expect(uploaded.length).toBe(1))
    expect(uploaded[0].name).toMatch(/^pasted-.*\.png$/)
  })

  it("says what to do instead where the browser will not, or has nothing it can read", async () => {
    page()
    const click = () => fireEvent.click(screen.getByRole("button", { name: "Paste from my clipboard" }))
    for (const [clipboard, text] of [
      [undefined, /does not let the page read your clipboard/],
      [{ read: async () => Promise.reject(new DOMException("no", "NotAllowedError")) }, /does not let the page read your clipboard/],
      [{ read: async () => [{ types: ["text/plain"], getType: async () => new Blob(["x"]) }] }, /no picture on your clipboard/],
    ] as const) {
      withClipboard(clipboard)
      click()
      expect((await screen.findByRole("alert")).textContent).toMatch(text)
      await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(text))
    }
    expect(names()).toEqual([])
    await expect(readClipboardFiles(undefined)).rejects.toBeInstanceOf(ClipboardReadError)
  })
})

describe("names and rules", () => {
  it("names pasted data by the time and its type", () => {
    const at = new Date(2026, 9, 2, 7, 31)
    expect(pastedName("image/png", at)).toBe("pasted-2026-10-02-0731.png")
    expect(pastedName("image/jpeg", at, 1)).toBe("pasted-2026-10-02-0731-2.jpg")
    expect(pastedName("application/x-unknown", at)).toBe("pasted-2026-10-02-0731.bin")
    const got = pastedFiles(transfer([file("image.png", "image/png"), file("report.pdf"), file("", "image/jpeg")]) as unknown as DataTransfer, at)
    expect(got.map(f => f.name)).toEqual(["pasted-2026-10-02-0731.png", "report.pdf", "pasted-2026-10-02-0731-2.jpg"])
  })

  it("is not for the session without files", () => {
    const event = new Event("paste") as ClipboardEvent
    expect(pasteIsForSession(event)).toBe(false)
  })
})
