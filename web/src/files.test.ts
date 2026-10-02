import { describe, expect, it } from "vitest"
import { ApiError, SignedOutError, createApi } from "./api"
import { fileErrorText, fileUrl, formatSize, safeFileName, uploadFile } from "./files"

const ID = "s-aaaaaaaaaa"

// An XMLHttpRequest that answers with status and body as soon as it is sent.
function fakeXhr(status: number, body: string, fail = false) {
  const xhr = {
    method: "",
    url: "",
    sent: undefined as unknown,
    status: 0,
    statusText: "",
    responseText: "",
    upload: { onprogress: null as ((e: ProgressEvent) => void) | null },
    onload: null as (() => void) | null,
    onerror: null as (() => void) | null,
    onabort: null as (() => void) | null,
    open(method: string, url: string) {
      xhr.method = method
      xhr.url = url
    },
    abort() {},
    send(data: unknown) {
      xhr.sent = data
      xhr.upload.onprogress?.({ lengthComputable: true, loaded: 1, total: 4 } as ProgressEvent)
      if (fail) return xhr.onerror?.()
      xhr.status = status
      xhr.responseText = body
      xhr.onload?.()
    },
  }
  return xhr
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const using = (xhr: ReturnType<typeof fakeXhr>) => () => xhr as any

describe("safeFileName", () => {
  it("keeps ordinary names", () => {
    for (const name of ["report.pdf", "my file (1).txt", "café.tar.gz", "a..b"]) expect(safeFileName(name)).toBe(name)
  })

  it("leaves nothing that could name another path, or a hidden file", () => {
    expect(safeFileName("../../etc/passwd")).toBe("_.._etc_passwd")
    expect(safeFileName("..\\x")).toBe("_x")
    expect(safeFileName(".bashrc")).toBe("bashrc")
    expect(safeFileName("a\u0000b\nc")).toBe("a_b_c")
    for (const name of ["", ".", "..", "   "]) expect(safeFileName(name)).toBe("file")
  })

  it("shortens a long name but keeps its extension", () => {
    const safe = safeFileName("é".repeat(400) + ".pdf")
    expect(new TextEncoder().encode(safe).length).toBeLessThanOrEqual(240)
    expect(safe.endsWith(".pdf")).toBe(true)
  })
})

describe("formatSize", () => {
  it("reads like a file manager", () => {
    expect([0, 1023, 1024, 1536, 10 * 1024, 100 << 20, 5 * 2 ** 30].map(formatSize)).toEqual([
      "0 B", "1023 B", "1.0 KB", "1.5 KB", "10 KB", "100 MB", "5.0 GB",
    ])
  })
})

describe("fileUrl", () => {
  it("escapes the name into one segment and refuses a malformed session", () => {
    expect(fileUrl(ID, "a b/c?#%.txt")).toBe(`/api/sessions/${ID}/files/a%20b%2Fc%3F%23%25.txt`)
    expect(() => fileUrl("../me", "a")).toThrow(ApiError)
  })
})

describe("uploadFile", () => {
  it("PUTs the file under its safe name and reports progress and the stored name", async () => {
    const xhr = fakeXhr(201, JSON.stringify({ name: "a b (1).txt", size: 4 }))
    const progress: number[] = []
    const blob = new Blob(["data"])
    await expect(uploadFile(ID, blob, "../a b.txt", f => progress.push(f), using(xhr))).resolves.toBe("a b (1).txt")
    expect(xhr.method).toBe("PUT")
    expect(xhr.url).toBe(`/api/sessions/${ID}/files/_a%20b.txt`)
    expect(xhr.sent).toBe(blob)
    expect(progress).toEqual([0.25, 1])
  })

  it("turns a refusal into an ApiError with the backend's message", async () => {
    const tooLarge = uploadFile(ID, new Blob(), "a", undefined, using(fakeXhr(413, JSON.stringify({ error: "file too large" }))))
    await expect(tooLarge).rejects.toMatchObject({ status: 413, message: "file too large" })
    await expect(uploadFile(ID, new Blob(), "a", undefined, using(fakeXhr(502, "<html>")))).rejects.toBeInstanceOf(ApiError)
  })

  it("reads a 401 as signed out and no answer as a lost connection", async () => {
    await expect(uploadFile(ID, new Blob(), "a", undefined, using(fakeXhr(401, "")))).rejects.toBeInstanceOf(SignedOutError)
    await expect(uploadFile(ID, new Blob(), "a", undefined, using(fakeXhr(0, "", true)))).rejects.toMatchObject({ status: 0 })
  })

  it("never sends for a malformed session id", async () => {
    const xhr = fakeXhr(201, "{}")
    await expect(uploadFile("nope", new Blob(), "a", undefined, using(xhr))).rejects.toBeInstanceOf(ApiError)
    expect(xhr.sent).toBeUndefined()
  })
})

describe("fileErrorText", () => {
  it("says what happened in the user's terms", () => {
    expect(fileErrorText(new ApiError(413, "file too large"), 100 << 20)).toBe("It is larger than 100 MB.")
    expect(fileErrorText(new ApiError(409, "x"))).toMatch(/asleep/)
    expect(fileErrorText(new ApiError(500, "boom"))).toBe("boom")
    expect(fileErrorText(new Error("x"))).toBe("Something went wrong.")
  })
})

describe("files api", () => {
  it("lists and deletes on the session's own API path", async () => {
    const calls: [string, string][] = []
    const fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push([init?.method ?? "GET", String(input)])
      return init?.method === "DELETE"
        ? new Response(null, { status: 204 })
        : new Response(JSON.stringify({ files: [{ name: "a.txt", size: 1, modified: "2026-10-01T00:00:00Z" }], max_bytes: 5 }), {
            headers: { "Content-Type": "application/json" },
          })
    }
    const api = createApi(fetch as typeof globalThis.fetch)
    await expect(api.listFiles(ID)).resolves.toMatchObject({ max_bytes: 5, files: [{ name: "a.txt" }] })
    await api.deleteFile(ID, "a/b c.txt")
    expect(calls).toEqual([
      ["GET", `/api/sessions/${ID}/files`],
      ["DELETE", `/api/sessions/${ID}/files/a%2Fb%20c.txt`],
    ])
    await expect(api.listFiles("../x")).rejects.toBeInstanceOf(ApiError)
  })
})
