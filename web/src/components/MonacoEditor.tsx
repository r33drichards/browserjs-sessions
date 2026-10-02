// The editor itself. Loaded with React.lazy (see PolicyEditor.tsx), so Monaco
// is fetched only by the pages that edit a policy.
import { useEffect, useRef } from "react"
import type { Marker } from "../policy/markers"
import { THEME, setupMonaco } from "../policy/monaco"
import { REGO_LANGUAGE_ID } from "../policy/rego"

export interface MonacoEditorProps {
  value: string
  onChange: (value: string) => void
  markers: Marker[] // from the server's validation
  onCursor?: (line: number, column: number) => void
  ariaLabel: string
  height?: number
}

const SERVER = "server"

export default function MonacoEditor(props: MonacoEditorProps) {
  const { value, markers, ariaLabel, height = 320 } = props
  const host = useRef<HTMLDivElement>(null)
  const editorRef = useRef<ReturnType<ReturnType<typeof setupMonaco>["editor"]["create"]> | null>(null)
  // The latest callbacks, so the editor is not rebuilt when a parent re-renders.
  const callbacks = useRef(props)
  callbacks.current = props

  useEffect(() => {
    const monaco = setupMonaco()
    const uri = monaco.Uri.parse("inmemory://policy/session.policy.rego")
    monaco.editor.getModel(uri)?.dispose()
    const model = monaco.editor.createModel(callbacks.current.value, REGO_LANGUAGE_ID, uri)
    const editor = monaco.editor.create(host.current!, {
      model,
      theme: THEME,
      ariaLabel: callbacks.current.ariaLabel,
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      fontFamily: "ui-monospace, Menlo, monospace",
      fontSize: 13,
      tabSize: 4,
      insertSpaces: false, // Rego is written with tabs (opa fmt)
      renderLineHighlight: "line",
      fixedOverflowWidgets: true,
      bracketPairColorization: { enabled: false }, // the wireframe has no colour
      overviewRulerLanes: 0,
      // Tab moves focus out after Ctrl+M (Monaco's own toggle); Escape is not trapped.
      accessibilitySupport: "auto",
    })
    editorRef.current = editor

    const subscriptions = [
      model.onDidChangeContent(() => callbacks.current.onChange(model.getValue())),
      editor.onDidChangeCursorPosition(e => callbacks.current.onCursor?.(e.position.lineNumber, e.position.column)),
    ]
    return () => {
      subscriptions.forEach(s => s.dispose())
      editor.dispose()
      model.dispose()
      editorRef.current = null
    }
  }, [])

  // A value set from outside (a preset, a copied policy, a reload) replaces
  // the text; the editor's own edits arrive here unchanged and are left alone.
  useEffect(() => {
    const model = editorRef.current?.getModel()
    if (model && model.getValue() !== value) model.setValue(value)
  }, [value])

  useEffect(() => {
    const monaco = setupMonaco()
    const model = editorRef.current?.getModel()
    if (!model) return
    monaco.editor.setModelMarkers(
      model,
      SERVER,
      markers.map(m => ({
        ...m,
        severity: m.severity === "error" ? monaco.MarkerSeverity.Error : monaco.MarkerSeverity.Warning,
      })),
    )
  }, [markers])

  return <div ref={host} className="wf-editor" style={{ height }} aria-label={ariaLabel} />
}
