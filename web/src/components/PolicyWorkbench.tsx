// The policy editor as the create page's split panel and the edit page both
// use it: the format switch, the editor beside the generated Rego, a status
// bar with a problems pane, and Test. It saves nothing itself.
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ColumnLayout from "@cloudscape-design/components/column-layout"
import Container from "@cloudscape-design/components/container"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Modal from "@cloudscape-design/components/modal"
import SegmentedControl from "@cloudscape-design/components/segmented-control"
import Select from "@cloudscape-design/components/select"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Textarea from "@cloudscape-design/components/textarea"
import { useEffect, useMemo, useRef, useState } from "react"
import { signedOutHandled } from "../auth/signedOut"
import { mergeProblems, problemLine, toMarkers } from "../policy/markers"
import { REGO_TEMPLATE } from "../policy/rego"
import { SAMPLES, sampleText } from "../policy/samples"
import type { Diagnostic, Evaluation, PolicyKind, PolicySource, Validation } from "../policyApi"
import { KIND_LABEL, ifAvailable, policyApi } from "../policyApi"
import { CodeView, PolicyEditor } from "./PolicyEditor"

export const JSON_TEMPLATE = `{
  "version": 1,
  "description": "",
  "allow": { "operations": ["*"] }
}
`

const VALIDATE_DELAY_MS = 400
const NONE: Diagnostic[] = []

export interface Problems {
  errors: Diagnostic[]
  warnings: Diagnostic[]
}

interface Props {
  draft: PolicySource
  onChange: (draft: PolicySource) => void
  // Managed as code: the source is shown, not edited. Test still works.
  readOnly?: boolean
  // The Rego in force, shown beside a read-only JSON policy.
  regoInForce?: string
  // What a refused save or create answered (422); shown until the next edit.
  refused?: Problems | null
  // The server's latest verdict on the draft; null while there is none.
  onValidation?: (validation: Validation | null) => void
  editorHeight?: number
  // Shown at the end of the format row (the edit page's Copy from session).
  tools?: React.ReactNode
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`

export function PolicyWorkbench({ draft, onChange, readOnly, regoInForce, refused, onValidation, editorHeight = 320, tools }: Props) {
  const { kind, source } = draft
  const [validation, setValidation] = useState<Validation | null>(null)
  const [checking, setChecking] = useState(false)
  const [checkError, setCheckError] = useState("") // the check itself failed; the policy may be fine
  const [local, setLocal] = useState<Diagnostic[]>([]) // found by the editor (JSON syntax and schema)
  const [cursor, setCursor] = useState<[number, number]>([1, 1])
  const [schema, setSchema] = useState<Record<string, unknown> | null>(null)
  const [switchTo, setSwitchTo] = useState<PolicyKind | null>(null) // the format change awaiting confirmation
  const onValidationRef = useRef(onValidation)
  onValidationRef.current = onValidation

  // The JSON form's schema, for the editor's own completion and squiggles.
  // Without it the server's check still reports everything.
  useEffect(() => {
    if (readOnly) return
    let cancelled = false
    ifAvailable(policyApi.schema())
      .then(s => !cancelled && setSchema(s))
      .catch(signedOutHandled)
    return () => {
      cancelled = true
    }
  }, [readOnly])

  // The operator's own check, a moment after the last keystroke: what the
  // editor shows is what a save would get.
  useEffect(() => {
    if (readOnly) return
    setValidation(null)
    onValidationRef.current?.(null)
    setCheckError("")
    if (!source.trim()) return setChecking(false)
    let cancelled = false
    setChecking(true)
    const timer = setTimeout(() => {
      policyApi
        .validate({ kind, source })
        .then(v => {
          if (cancelled) return
          setValidation(v)
          onValidationRef.current?.(v)
        })
        .catch(e => {
          if (cancelled || signedOutHandled(e)) return
          setCheckError(String(e instanceof Error ? e.message : e))
        })
        .finally(() => !cancelled && setChecking(false))
    }, VALIDATE_DELAY_MS)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [kind, source, readOnly])

  const serverErrors = validation?.errors ?? refused?.errors ?? NONE
  const serverWarnings = validation?.warnings ?? refused?.warnings ?? NONE
  const markers = useMemo(() => toMarkers(serverErrors, serverWarnings, source), [serverErrors, serverWarnings, source])
  const reported = [...serverErrors, ...serverWarnings]
  const errors = mergeProblems(serverErrors, local.filter(d => d.code === "json_error"), reported)
  const warnings = mergeProblems(serverWarnings, local.filter(d => d.code !== "json_error"), reported)

  const generated = readOnly ? regoInForce : validation?.rego

  function confirmSwitch() {
    if (!switchTo) return
    // JSON becomes its generated Rego when there is one; Rego cannot become JSON.
    onChange({ kind: switchTo, source: switchTo === "rego" ? (validation?.rego ?? REGO_TEMPLATE) : JSON_TEMPLATE })
    setSwitchTo(null)
  }

  const fileName = kind === "json" ? "policy.json" : "policy.rego"

  return (
    <SpaceBetween size="m">
      {!readOnly && (
        <div className="wf-row">
          <FormField label="Format" description="JSON is compiled to Rego. Rego is used as written.">
            <SegmentedControl
              label="Policy format"
              selectedId={kind}
              options={[
                { id: "json", text: "JSON" },
                { id: "rego", text: "Rego" },
              ]}
              onChange={e => {
                const next = e.detail.selectedId as PolicyKind
                if (next !== kind) setSwitchTo(next)
              }}
            />
          </FormField>
          {tools}
        </div>
      )}

      <ColumnLayout columns={kind === "json" ? 2 : 1}>
        <div>
          <Box variant="awsui-key-label">
            {fileName}
            {readOnly ? " (read-only)" : ""}
          </Box>
          {readOnly ? (
            <CodeView code={source} label={`${fileName}, read-only`} height={editorHeight} />
          ) : (
            <PolicyEditor
              kind={kind}
              value={source}
              onChange={next => onChange({ kind, source: next })}
              markers={markers}
              schema={kind === "json" ? schema : null}
              onCursor={(line, column) => setCursor([line, column])}
              onLocalProblems={setLocal}
              ariaLabel={`${KIND_LABEL[kind]} policy editor`}
              height={editorHeight}
            />
          )}
        </div>
        {kind === "json" && (
          <div>
            <Box variant="awsui-key-label">Generated Rego (read-only)</Box>
            <CodeView
              code={generated ?? (checking ? "Checking the policy" : "Shown when the JSON policy is valid.")}
              label="Generated Rego, read-only"
              height={editorHeight}
            />
          </div>
        )}
      </ColumnLayout>

      {!readOnly && (
        <div className="wf-statusbar" role="status" aria-live="polite">
          <span>{KIND_LABEL[kind]}</span>
          <span>
            Ln {cursor[0]}, Col {cursor[1]}
          </span>
          <span>{plural(errors.length, "error")}</span>
          <span>{plural(warnings.length, "warning")}</span>
          <span className="wf-note">
            {checking ? "Checking" : checkError ? `Couldn't check the policy: ${checkError}` : validation?.ok ? "Valid" : ""}
          </span>
        </div>
      )}
      {!readOnly && errors.length + warnings.length > 0 && (
        <ul className="wf-problems" aria-label="Problems">
          {errors.map((d, i) => (
            <li key={`e${i}`}>
              <b>Error</b> {problemLine(d)}
            </li>
          ))}
          {warnings.map((d, i) => (
            <li key={`w${i}`}>
              <b>Warning</b> {problemLine(d)}
            </li>
          ))}
        </ul>
      )}

      <PolicyTest draft={draft} />

      {switchTo !== null && (
        <Modal
          visible
          onDismiss={() => setSwitchTo(null)}
          header={switchTo === "rego" ? "Convert to Rego" : "Switch to JSON"}
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setSwitchTo(null)}>
                  Cancel
                </Button>
                <Button variant="primary" onClick={confirmSwitch}>
                  {switchTo === "rego" ? "Convert to Rego" : "Switch to JSON"}
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          {switchTo === "rego"
            ? validation?.rego
              ? "The editor will hold the Rego generated from this JSON policy. Rego can't be converted back to JSON."
              : "This JSON policy has no generated Rego yet, so the editor will start from a Rego template and the JSON is discarded. Rego can't be converted back to JSON."
            : "Rego can't be converted to JSON. The editor will start from a JSON policy with no restrictions and the Rego is discarded."}
        </Modal>
      )}
    </SpaceBetween>
  )
}

// Runs the policy in the editor against a sample call. Nothing is saved.
function PolicyTest({ draft }: { draft: PolicySource }) {
  const [sampleId, setSampleId] = useState(SAMPLES[0].id)
  const [input, setInput] = useState(() => sampleText(SAMPLES[0]))
  const [result, setResult] = useState<Evaluation | null>(null)
  const [error, setError] = useState("")
  const [running, setRunning] = useState(false)

  // A result belongs to the policy and the call it was run with.
  useEffect(() => {
    setResult(null)
    setError("")
  }, [draft.kind, draft.source, input])

  async function run() {
    setResult(null)
    let parsed: unknown
    try {
      parsed = JSON.parse(input)
    } catch (e) {
      return setError(`The sample call is not valid JSON: ${(e as Error).message}`)
    }
    setError("")
    setRunning(true)
    try {
      setResult(await policyApi.evaluate(draft, parsed))
    } catch (e) {
      if (!signedOutHandled(e)) setError(String(e instanceof Error ? e.message : e))
    } finally {
      setRunning(false)
    }
  }

  const options = SAMPLES.map(s => ({ value: s.id, label: s.label }))

  return (
    <Container
      header={
        <Header variant="h3" description="Runs the policy in the editor against a call, with the same checks as the real thing. Nothing is saved.">
          Test
        </Header>
      }
    >
      <SpaceBetween size="s">
        <div className="wf-row">
          <FormField label="Sample call">
            <Select
              selectedOption={options.find(o => o.value === sampleId) ?? null}
              options={options}
              onChange={e => {
                const sample = SAMPLES.find(s => s.id === e.detail.selectedOption.value) ?? SAMPLES[0]
                setSampleId(sample.id)
                setInput(sampleText(sample))
              }}
            />
          </FormField>
          <Button loading={running} formAction="none" onClick={run}>
            Run test
          </Button>
        </div>
        <FormField label="Call" errorText={error}>
          <Textarea value={input} onChange={e => setInput(e.detail.value)} rows={6} spellcheck={false} ariaLabel="Sample call" />
        </FormField>
        {result && (
          <div role="status">
            <Box variant="awsui-key-label">Result</Box>
            {result.ok ? (
              <span className="wf-state" data-verdict={result.allow ? "allow" : "deny"}>
                {result.allow ? "Allowed" : "Denied"}
              </span>
            ) : (
              <ul className="wf-problems">
                {(result.errors ?? []).map((d, i) => (
                  <li key={i}>
                    <b>Error</b> {problemLine(d)}
                  </li>
                ))}
                {(result.errors ?? []).length === 0 && <li>The policy could not be evaluated.</li>}
              </ul>
            )}
          </div>
        )}
      </SpaceBetween>
    </Container>
  )
}
