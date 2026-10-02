import Button from "@cloudscape-design/components/button"
import { Component, Suspense, lazy } from "react"
import type { MonacoEditorProps } from "./MonacoEditor"

// Monaco is a chunk of its own: only a page that edits a policy fetches it.
const MonacoEditor = lazy(() => import("./MonacoEditor"))

// The chunk can fail to load (offline, or a deploy replaced it): say so and
// offer the reload that fetches the current build.
class EditorBoundary extends Component<{ children: React.ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() {
    return { failed: true }
  }
  render() {
    if (!this.state.failed) return this.props.children
    return (
      <div className="wf-editor wf-editor-note" role="alert">
        <p>The editor couldn&apos;t be loaded.</p>
        <Button onClick={() => window.location.reload()}>Reload the page</Button>
      </div>
    )
  }
}

export function PolicyEditor(props: MonacoEditorProps) {
  return (
    <EditorBoundary>
      <Suspense
        fallback={
          <div className="wf-editor wf-editor-note" style={{ height: props.height ?? 320 }}>
            <span className="wf-spinner" aria-hidden="true" />
            <p>Loading the editor</p>
          </div>
        }
      >
        <MonacoEditor {...props} />
      </Suspense>
    </EditorBoundary>
  )
}

// Code that is only read (the generated Rego, a policy managed as code): a
// plain block, not an editor.
export function CodeView({ code, label, height }: { code: string; label: string; height?: number }) {
  return (
    <pre className="wf-code" aria-label={label} tabIndex={0} style={height ? { height } : undefined}>
      {code}
    </pre>
  )
}
