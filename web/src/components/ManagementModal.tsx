// Who manages the session's policy: this editor, or code (Terraform, the
// API). The two are exclusive; either can be chosen at any time.
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import FormField from "@cloudscape-design/components/form-field"
import Input from "@cloudscape-design/components/input"
import Modal from "@cloudscape-design/components/modal"
import RadioGroup from "@cloudscape-design/components/radio-group"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useState } from "react"
import { signedOutHandled } from "../auth/signedOut"
import type { Management, ManagementMode, Policy } from "../policyApi"
import { managedUrlError, policyApi } from "../policyApi"

interface Props {
  sessionId: string
  current: Management
  initialMode?: ManagementMode // "Manage here instead" opens on the editor
  onDismiss: () => void
  onChanged: (policy: Policy) => void
}

// Mounted only while open, so it always starts from the current mode.
export function ManagementModal({ sessionId, current, initialMode, onDismiss, onChanged }: Props) {
  const [mode, setMode] = useState<ManagementMode>(initialMode ?? current.mode)
  const [url, setUrl] = useState(current.managed_url ?? "")
  const [urlError, setUrlError] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  async function save() {
    if (busy) return
    setError("")
    const problem = mode === "iac" ? managedUrlError(url) : ""
    setUrlError(problem)
    if (problem) return
    setBusy(true)
    try {
      onChanged(await policyApi.setManagement(sessionId, mode === "iac" ? { mode, managed_url: url.trim() } : { mode }))
    } catch (e) {
      if (!signedOutHandled(e)) setError(String(e instanceof Error ? e.message : e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      visible
      onDismiss={onDismiss}
      header="Policy management"
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onDismiss}>
              Cancel
            </Button>
            <Button variant="primary" loading={busy} onClick={save}>
              Save management
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <FormField label="Managed in" errorText={error}>
          <RadioGroup
            ariaLabel="Managed in"
            value={mode}
            onChange={e => setMode(e.detail.value as ManagementMode)}
            items={[
              {
                value: "editor",
                label: "This editor",
                description: "The policy is edited here. Writes made with an API token are refused.",
              },
              {
                value: "iac",
                label: "Code",
                description:
                  "Terraform, OpenTofu or the API writes the policy. Here it is read-only, with a link to where it is edited.",
              },
            ]}
          />
        </FormField>
        {mode === "iac" && (
          <FormField label="Link to where it is managed" constraintText="Starts with https://." errorText={urlError}>
            <Input value={url} type="url" placeholder="https://" onChange={e => setUrl(e.detail.value)} />
          </FormField>
        )}
        {mode === "editor" && current.mode === "iac" && (
          <Box color="text-body-secondary">
            The code that manages this policy will see the switch as drift, and can&apos;t write the policy again until it
            is managed as code.
          </Box>
        )}
      </SpaceBetween>
    </Modal>
  )
}
