// Files that reach the session page other than through the file chooser:
// pasted from this computer's clipboard, or dropped anywhere on the page.

// Where the remote screen's canvas lives (VncPane). Keys pressed there are
// the remote browser's, Ctrl+V included: that is how a file put on its
// clipboard is pasted into a page there.
export const REMOTE_SCREEN = ".wf-screen"

const EXTENSIONS: Record<string, string> = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/gif": "gif",
  "image/webp": "webp",
  "image/svg+xml": "svg",
  "image/bmp": "bmp",
  "image/tiff": "tiff",
  "application/pdf": "pdf",
  "text/plain": "txt",
}

const two = (n: number) => String(n).padStart(2, "0")

// "pasted-2026-10-02-0731.png": a name for clipboard data that has none.
export function pastedName(type: string, now = new Date(), nth = 0): string {
  const stamp = `${now.getFullYear()}-${two(now.getMonth() + 1)}-${two(now.getDate())}-${two(now.getHours())}${two(now.getMinutes())}`
  const ext = EXTENSIONS[type.split(";")[0].trim().toLowerCase()] ?? "bin"
  return `pasted-${stamp}${nth > 0 ? `-${nth + 1}` : ""}.${ext}`
}

// A screenshot or a copied picture arrives as a file the browser called
// "image.png"; a file copied in a file manager keeps its own name.
const unnamed = (name: string) => name === "" || /^image\.[a-z0-9]+$/i.test(name)

function named(files: File[], now: Date): File[] {
  let nth = 0
  return files.map(f => (unnamed(f.name) ? new File([f], pastedName(f.type, now, nth++), { type: f.type }) : f))
}

// The files a paste carries: copied files, and pictures (which some browsers
// list only as items of kind "file").
export function pastedFiles(data: DataTransfer | null, now = new Date()): File[] {
  if (!data) return []
  let files = Array.from(data.files ?? [])
  if (files.length === 0) {
    files = Array.from(data.items ?? [])
      .filter(item => item.kind === "file")
      .map(item => item.getAsFile())
      .filter((f): f is File => f !== null)
  }
  return named(files, now)
}

const hasText = (data: DataTransfer) => Array.from(data.types ?? []).includes("text/plain")

function isTextField(el: Element | null): boolean {
  if (!el) return false
  if (el instanceof HTMLTextAreaElement) return true
  if (el instanceof HTMLInputElement) return el.type !== "file" && el.type !== "button" && el.type !== "checkbox"
  return el instanceof HTMLElement && el.isContentEditable
}

// Whether a paste on the session page is files for the session, and not
//  - a key press for the remote browser (the focus is in its screen), or
//  - text for a field of the page (a spreadsheet's cells are copied as text
//    and as a picture of them: a text field gets the text).
export function pasteIsForSession(event: ClipboardEvent, active: Element | null = document.activeElement): boolean {
  const data = event.clipboardData
  if (!data || pastedFiles(data).length === 0) return false
  const target = event.target instanceof Element ? event.target : null
  if (target?.closest(REMOTE_SCREEN) || active?.closest(REMOTE_SCREEN)) return false
  if ((isTextField(target) || isTextField(active)) && hasText(data)) return false
  return true
}

export const dragHasFiles = (data: DataTransfer | null) => Array.from(data?.types ?? []).includes("Files")

export class ClipboardReadError extends Error {
  constructor(public reason: "unsupported" | "denied" | "empty") {
    super(reason)
  }
}

// What the browser lets a page read from the clipboard when asked (a
// button, not a paste): pictures, and in some browsers nothing at all.
// A file copied in a file manager is not among them; that takes a paste.
export async function readClipboardFiles(
  clipboard: Pick<Clipboard, "read"> | undefined = navigator.clipboard,
  now = new Date(),
): Promise<File[]> {
  if (!clipboard || typeof clipboard.read !== "function") throw new ClipboardReadError("unsupported")
  let items: ClipboardItems
  try {
    items = await clipboard.read()
  } catch {
    throw new ClipboardReadError("denied")
  }
  const files: File[] = []
  for (const item of items) {
    const type = item.types.find(t => t.startsWith("image/")) ?? item.types.find(t => t === "application/pdf")
    if (!type) continue
    try {
      const blob = await item.getType(type)
      files.push(new File([blob], pastedName(type, now, files.length), { type }))
    } catch {} // announced but not handed over
  }
  if (files.length === 0) throw new ClipboardReadError("empty")
  return files
}
