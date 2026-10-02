import Button from "@cloudscape-design/components/button"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useRef, useState } from "react"
import { api } from "../api"
import type { SessionFile } from "../api"
import { ApiError } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { fileErrorText, fileUrl, formatSize, uploadFile } from "../files"
import { usePolling } from "../usePolling"
import "./FilesBox.css"

interface Upload {
  key: number
  name: string
  fraction: number
}

const DEFAULT_MAX_BYTES = 100 * 1024 * 1024
const POLL_MS = 5000

// The clipboard carries text; this carries files. They go to, and come from,
// the one folder the session's browser downloads to and opens its file
// chooser in.
export function FilesBox({ sessionId }: { sessionId: string }) {
  const [files, setFiles] = useState<SessionFile[] | null>(null)
  const [maxBytes, setMaxBytes] = useState(DEFAULT_MAX_BYTES)
  const [unavailable, setUnavailable] = useState("") // why there is no list, if there is none
  const [uploads, setUploads] = useState<Upload[]>([])
  const [errors, setErrors] = useState<string[]>([])
  const [over, setOver] = useState(false)
  const [note, setNote] = useState("") // what is on the browser's clipboard, as far as this page put it there
  const input = useRef<HTMLInputElement>(null)
  const nextKey = useRef(0)
  // Uploads go one at a time, in the order they were dropped.
  const queue = useRef<Promise<void>>(Promise.resolve())
  const gone = useRef(false)
  useEffect(() => {
    gone.current = false
    return () => {
      gone.current = true
    }
  }, [sessionId])

  const load = useCallback(async () => {
    try {
      const listed = await api.listFiles(sessionId)
      if (gone.current) return
      setFiles(listed.files)
      setMaxBytes(listed.max_bytes)
      setUnavailable("")
    } catch (err) {
      if (signedOutHandled(err) || gone.current) return
      // 501: a session from before its browser served files.
      const status = err instanceof ApiError ? err.status : 0
      setUnavailable(status === 501 && err instanceof ApiError ? err.message : `Can't list the files right now. ${fileErrorText(err)}`)
    }
  }, [sessionId])
  usePolling(load, true, POLL_MS)

  // Puts files of the folder on the browser's clipboard, as one selection.
  async function copy(names: string[]) {
    try {
      await api.copyFiles(sessionId, names)
      if (gone.current) return
      const what = names.length === 1 ? names[0] : `${names.length} files`
      setNote(`${what} on the browser's clipboard. Press Ctrl+V in the browser to paste.`)
    } catch (err) {
      if (signedOutHandled(err) || gone.current) return
      setNote("")
      const text = err instanceof ApiError && err.status === 501 ? err.message : fileErrorText(err)
      setErrors(e => [...e, `Not put on the browser's clipboard. ${text}`])
    }
  }

  function send(chosen: File[]) {
    setErrors([])
    const stored: string[] = [] // the names this batch got in the folder
    for (const file of chosen) {
      if (file.size > maxBytes) {
        setErrors(e => [...e, `${file.name} was not sent. It is larger than ${formatSize(maxBytes)}.`])
        continue
      }
      const key = nextKey.current++
      setUploads(u => [...u, { key, name: file.name, fraction: 0 }])
      queue.current = queue.current.then(async () => {
        if (gone.current) return
        try {
          stored.push(
            await uploadFile(sessionId, file, file.name, fraction => {
              if (!gone.current) setUploads(u => u.map(x => (x.key === key ? { ...x, fraction } : x)))
            }),
          )
        } catch (err) {
          // A folder dropped here has no bytes to read: the browser fails the request.
          if (!signedOutHandled(err) && !gone.current)
            setErrors(e => [...e, `${file.name} was not sent. ${fileErrorText(err, maxBytes)}`])
        }
        if (gone.current) return
        setUploads(u => u.filter(x => x.key !== key))
        await load()
      })
    }
    // What was dropped together goes on the clipboard together; the last
    // batch wins.
    queue.current = queue.current.then(async () => {
      if (!gone.current && stored.length > 0) await copy(stored)
    })
  }

  async function remove(file: SessionFile) {
    if (!window.confirm(`Delete "${file.name}" from the session? This cannot be undone.`)) return
    setErrors([])
    setNote("") // if it was on the clipboard, it no longer is
    try {
      await api.deleteFile(sessionId, file.name)
    } catch (err) {
      if (signedOutHandled(err)) return
      // Already gone is what was asked for.
      if (!(err instanceof ApiError && err.status === 404))
        setErrors([`${file.name} was not deleted. ${fileErrorText(err)}`])
    }
    await load()
  }

  const hasFiles = (e: React.DragEvent) => Array.from(e.dataTransfer.types).includes("Files")

  return (
    <div
      className={over ? "wf-files wf-files-over" : "wf-files"}
      onDragOver={e => {
        if (!hasFiles(e)) return
        e.preventDefault()
        e.dataTransfer.dropEffect = "copy"
        setOver(true)
      }}
      onDragLeave={e => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(false)
      }}
      onDrop={e => {
        if (!hasFiles(e)) return
        e.preventDefault()
        setOver(false)
        send(Array.from(e.dataTransfer.files))
      }}
    >
      <SpaceBetween size="xs">
        <strong>Files</strong>
        <span className="wf-note">
          Drop files here to put them in the browser's Downloads folder, where its file chooser opens. What the browser
          downloads shows up here to save. A file you send is also copied to the browser's clipboard, so Ctrl+V there
          pastes it. Up to {formatSize(maxBytes)} each.
        </span>
        <SpaceBetween direction="horizontal" size="xs" alignItems="center">
          <Button iconName="upload" disabled={Boolean(unavailable) && files === null} onClick={() => input.current?.click()}>
            Send files to browser
          </Button>
          <input
            ref={input}
            type="file"
            multiple
            aria-label="Files to send to the browser"
            onChange={e => {
              send(Array.from(e.target.files ?? []))
              e.target.value = "" // so the same file can be chosen again
            }}
          />
          {unavailable ? <span className="wf-note">{unavailable}</span> : null}
          {note && !unavailable ? (
            <span className="wf-note" role="status">
              {note}
            </span>
          ) : null}
        </SpaceBetween>
        {errors.map((text, i) => (
          <div key={i} className="wf-files-error" role="alert">
            ⚠ {text}
          </div>
        ))}
        {(uploads.length > 0 || (files && files.length > 0)) && (
          <ul className="wf-files-list">
            {uploads.map(u => (
              <li key={`up-${u.key}`}>
                <span className="wf-files-name">{u.name}</span>
                <progress max={1} value={u.fraction} aria-label={`Sending ${u.name}`} />
                <span className="wf-note">{u.fraction > 0 ? `${Math.round(u.fraction * 100)}%` : "waiting"}</span>
              </li>
            ))}
            {(files ?? []).map(f => (
              <li key={f.name}>
                <span className="wf-files-name" title={f.name}>
                  {f.name}
                </span>
                <span className="wf-note">{formatSize(f.size)}</span>
                <Button iconName="download" href={fileUrl(sessionId, f.name)} download={f.name} ariaLabel={`Save ${f.name}`}>
                  Save
                </Button>
                <Button iconName="copy" ariaLabel={`Copy ${f.name} to the browser's clipboard`} onClick={() => copy([f.name])}>
                  Copy to browser clipboard
                </Button>
                <Button iconName="remove" ariaLabel={`Delete ${f.name}`} onClick={() => remove(f)}>
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        )}
        {files && files.length === 0 && uploads.length === 0 && <span className="wf-note">No files in the session yet.</span>}
      </SpaceBetween>
    </div>
  )
}
