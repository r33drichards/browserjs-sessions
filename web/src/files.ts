import { ApiError, SignedOutError, isSessionId } from "./api"

// What the backend takes for a file name: one path segment, not hidden.
// Anything else in the name of a file on this computer is replaced, not
// refused.
export function safeFileName(name: string): string {
  // eslint-disable-next-line no-control-regex
  let safe = name.replace(/[/\\\u0000-\u001f\u007f]/g, "_").replace(/^\.+/, "").trim()
  if (!safe) safe = "file"
  // At most 255 bytes, less room for the " (12)" of a name already taken;
  // the extension is what survives.
  const bytes = (s: string) => new TextEncoder().encode(s).length
  const dot = safe.lastIndexOf(".")
  const ext = dot > 0 && safe.length - dot <= 16 ? safe.slice(dot) : ""
  let stem = safe.slice(0, safe.length - ext.length)
  while (bytes(stem + ext) > 240) stem = stem.slice(0, -1)
  return stem + ext
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ["KB", "MB", "GB", "TB"]
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
}

// Where one file of a session is fetched from and sent to. Following it as a
// link saves the file: the backend only ever sends it as an attachment.
export function fileUrl(id: string, name: string): string {
  if (!isSessionId(id)) throw new ApiError(404, "session not found")
  return `/api/sessions/${id}/files/${encodeURIComponent(name)}`
}

// Why a file could not be moved, for the user.
export function fileErrorText(error: unknown, maxBytes?: number): string {
  if (!(error instanceof ApiError)) return "Something went wrong."
  switch (error.status) {
    case 0:
      return "The connection was lost."
    case 404:
      return "It is no longer there."
    case 409:
      return "The session is asleep or stopped."
    case 413:
      return maxBytes ? `It is larger than ${formatSize(maxBytes)}.` : "It is too large."
    case 507:
      return "The session's disk is full."
    default:
      return error.message
  }
}

// Sends file to the session's folder as name, reporting how much of it has
// gone (0 to 1; fetch cannot). Resolves with the name it was given there, which differs
// if that name was taken: nothing is replaced.
export function uploadFile(
  id: string,
  file: Blob,
  name: string,
  onProgress: (fraction: number) => void = () => {},
  newXhr: () => XMLHttpRequest = () => new XMLHttpRequest(),
): Promise<string> {
  return new Promise((resolve, reject) => {
    const xhr = newXhr()
    xhr.open("PUT", fileUrl(id, safeFileName(name)))
    xhr.upload.onprogress = e => {
      if (e.lengthComputable && e.total > 0) onProgress(e.loaded / e.total)
    }
    xhr.onload = () => {
      if (xhr.status === 401) return reject(new SignedOutError())
      if (xhr.status < 200 || xhr.status >= 300) {
        let message = `${xhr.status} ${xhr.statusText}`.trim()
        try {
          message = JSON.parse(xhr.responseText).error ?? message
        } catch {}
        return reject(new ApiError(xhr.status, message))
      }
      onProgress(1)
      let stored = safeFileName(name)
      try {
        const answered = JSON.parse(xhr.responseText).name
        if (typeof answered === "string" && answered) stored = answered
      } catch {}
      resolve(stored)
    }
    // No answer at all: the network, or the identity proxy redirecting an
    // expired sign-in somewhere this request may not follow.
    xhr.onerror = () => reject(new ApiError(0, "connection lost"))
    xhr.onabort = () => reject(new ApiError(0, "cancelled"))
    xhr.send(file)
  })
}
