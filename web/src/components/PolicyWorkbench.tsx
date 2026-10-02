// The policy editor as the create page's split panel and the edit page both
// use it: the Rego editor, a status bar with a problems pane, and Test. It
// saves nothing itself.
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ButtonDropdown from "@cloudscape-design/components/button-dropdown"
import Container from "@cloudscape-design/components/container"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Select from "@cloudscape-design/components/select"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Textarea from "@cloudscape-design/components/textarea"
import { useEffect, useMemo, useRef, useState } from "react"
import { signedOutHandled } from "../auth/signedOut"
import { problemLine, toMarkers } from "../policy/markers"
import { SAMPLES, SAMPLE_GROUPS, sampleText } from "../policy/samples"
import type { Diagnostic, Evaluation, PolicySource, Preset, Validation } from "../policyApi"
import { policyApi } from "../policyApi"
import { CodeView, PolicyEditor } from "./PolicyEditor"

const VALIDATE_DELAY_MS = 400
const NONE: Diagnostic[] = []
const FILE_NAME = "policy.rego"

export interface Problems {
  errors: Diagnostic[]
  warnings: Diagnostic[]
}

interface Props {
  draft: PolicySource
  onChange: (draft: PolicySource) => void
  // Managed as code: the source is shown, not edited. Test still works.
  readOnly?: boolean
  // The module in force, shown under a read-only source that is not it (a
  // source that does not compile leaves the last one that did in force).
  regoInForce?: string
  // What a refused save or create answered (422); shown until the next edit.
  refused?: Problems | null
  // The server's latest verdict on the draft; null while there is none.
  onValidation?: (validation: Validation | null) => void
  editorHeight?: number
  // The ready-made policies: choosing one replaces the text in the editor.
  presets?: Preset[] | null
  // Shown beside the presets (the edit page's Copy from session).
  tools?: React.ReactNode
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`

export function PolicyWorkbench({ draft, onChange, readOnly, regoInForce, refused, onValidation, editorHeight = 320, presets, tools }: Props) {
  const { kind, source } = draft
  const [validation, setValidation] = useState<Validation | null>(null)
  const [checking, setChecking] = useState(false)
  const [checkError, setCheckError] = useState("") // the check itself failed; the policy may be fine
  const [cursor, setCursor] = useState<[number, number]>([1, 1])
  const onValidationRef = useRef(onValidation)
  onValidationRef.current = onValidation

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

  // Errors and warnings as the server gave them. The ones with a place are
  // also markers in the editor; a warning about the policy as a whole (one
  // tool restricted, another left open that walks around it) has no place
  // and is in the problems pane alone.
  const errors = validation?.errors ?? refused?.errors ?? NONE
  const warnings = validation?.warnings ?? refused?.warnings ?? NONE
  const markers = useMemo(() => toMarkers(errors, warnings, source), [errors, warnings, source])
  const inForce = readOnly && regoInForce && regoInForce !== source ? regoInForce : ""

  return (
    <SpaceBetween size="m">
      {!readOnly && ((presets && presets.length > 0) || tools) && (
        <div className="wf-row">
          {presets && presets.length > 0 && (
            <ButtonDropdown
              items={presets.map(p => ({ id: p.id, text: p.title, description: p.description }))}
              onItemClick={e => {
                const preset = presets.find(p => p.id === e.detail.id)
                if (preset) onChange({ kind: preset.kind, source: preset.source })
              }}
            >
              Start from a preset
            </ButtonDropdown>
          )}
          {tools}
        </div>
      )}

      <div>
        <Box variant="awsui-key-label">
          {FILE_NAME}
          {readOnly ? " (read-only)" : ""}
        </Box>
        {readOnly ? (
          <CodeView code={source} label={`${FILE_NAME}, read-only`} height={editorHeight} />
        ) : (
          <PolicyEditor
            value={source}
            onChange={next => onChange({ kind, source: next })}
            markers={markers}
            onCursor={(line, column) => setCursor([line, column])}
            ariaLabel="Rego policy editor"
            height={editorHeight}
          />
        )}
      </div>
      {inForce && (
        <div>
          <Box variant="awsui-key-label">Policy in force (read-only)</Box>
          <CodeView code={inForce} label="Policy in force, read-only" height={editorHeight} />
        </div>
      )}

      {!readOnly && (
        <div className="wf-statusbar" role="status" aria-live="polite">
          <span>Rego</span>
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

  const groups = SAMPLE_GROUPS.map(g => ({ label: g.label, options: g.samples.map(s => ({ value: s.id, label: s.label })) }))
  const options = groups.flatMap(g => g.options)

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
              options={groups}
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
          <Textarea value={input} onChange={e => setInput(e.detail.value)} rows={8} spellcheck={false} ariaLabel="Sample call" />
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
