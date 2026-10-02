// The editor itself. Loaded with React.lazy (see PolicyEditor.tsx), so Monaco
// is fetched only by the pages that edit a policy.
import { useEffect, useRef } from "react"
import type { Marker } from "../policy/markers"
import { JSON_MODEL_PATH, THEME, setPolicySchema, setupMonaco } from "../policy/monaco"
import { REGO_LANGUAGE_ID } from "../policy/rego"
import type { Diagnostic, PolicyKind } from "../policyApi"

export interface MonacoEditorProps {
  kind: PolicyKind
  value: string
  onChange: (value: string) => void
  markers: Marker[] // from the server's validation
  schema?: Record<string, unknown> | null // of the JSON form
  onCursor?: (line: number, column: number) => void
  // What the editor found by itself (JSON syntax and schema), as it changes.
  onLocalProblems?: (problems: Diagnostic[]) => void
  ariaLabel: string
  height?: number
}

const SERVER = "server"

export default function MonacoEditor(props: MonacoEditorProps) {
  const { kind, value, markers, schema, ariaLabel, height = 320 } = props
  const host = useRef<HTMLDivElement>(null)
  const editorRef = useRef<ReturnType<ReturnType<typeof setupMonaco>["editor"]["create"]> | null>(null)
  // The latest callbacks, so the editor is not rebuilt when a parent re-renders.
  const callbacks = useRef(props)
  callbacks.current = props

  // One editor per format: the model's language and name belong to it.
  useEffect(() => {
    const monaco = setupMonaco()
    const uri = monaco.Uri.parse(`inmemory://policy/${kind === "json" ? JSON_MODEL_PATH : "session.policy.rego"}`)
    monaco.editor.getModel(uri)?.dispose()
    const model = monaco.editor.createModel(callbacks.current.value, kind === "json" ? "json" : REGO_LANGUAGE_ID, uri)
    const editor = monaco.editor.create(host.current!, {
      model,
      theme: THEME,
      ariaLabel: callbacks.current.ariaLabel,
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      fontFamily: "ui-monospace, Menlo, monospace",
      fontSize: 13,
      tabSize: kind === "json" ? 2 : 4,
      insertSpaces: kind === "json", // Rego is written with tabs (opa fmt)
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
      monaco.editor.onDidChangeMarkers(uris => {
        if (!uris.some(u => u.toString() === uri.toString())) return
        const local = monaco.editor
          .getModelMarkers({ resource: uri })
          .filter(m => m.owner !== SERVER && m.severity >= monaco.MarkerSeverity.Warning)
          .map(m => ({
            row: m.startLineNumber,
            col: m.startColumn,
            code: m.severity === monaco.MarkerSeverity.Error ? "json_error" : "json_warning",
            message: m.message,
          }))
        callbacks.current.onLocalProblems?.(local)
      }),
    ]
    return () => {
      subscriptions.forEach(s => s.dispose())
      editor.dispose()
      model.dispose()
      editorRef.current = null
      callbacks.current.onLocalProblems?.([])
    }
  }, [kind])

  // A value set from outside (a copied policy, a reload) replaces the text;
  // the editor's own edits arrive here unchanged and are left alone.
  useEffect(() => {
    const model = editorRef.current?.getModel()
    if (model && model.getValue() !== value) model.setValue(value)
  }, [value, kind])

  useEffect(() => {
    if (schema) setPolicySchema(schema)
  }, [schema])

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
  }, [markers, kind])

  return <div ref={host} className="wf-editor" style={{ height }} aria-label={ariaLabel} />
}
