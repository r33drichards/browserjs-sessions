import { useEffect, useState } from "react"
import Alert from "@cloudscape-design/components/alert"
import Button from "@cloudscape-design/components/button"
import FormField from "@cloudscape-design/components/form-field"
import Input from "@cloudscape-design/components/input"
import Textarea from "@cloudscape-design/components/textarea"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { api } from "../api"

interface Settings {
  url: string
  filter?: string
  batch_size?: number
  flush_interval_seconds?: number
  has_signing_secret?: boolean
}

export function WebhookTab({ sessionId }: { sessionId: string }) {
  const [url, setUrl] = useState("")
  const [filter, setFilter] = useState("")
  const [size, setSize] = useState("100")
  const [interval, setInterval] = useState("5")
  const [secret, setSecret] = useState("")
  const [hasSecret, setHasSecret] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [message, setMessage] = useState("")
  useEffect(() => {
    let active = true
    setLoading(true)
    api.getWebhook<Settings | null>(sessionId).then(doc => {
      if (!active) return
      setUrl(doc?.url ?? ""); setFilter(doc?.filter ?? "")
      setSize(String(doc?.batch_size ?? 100)); setInterval(String(doc?.flush_interval_seconds ?? 5))
      setHasSecret(doc?.has_signing_secret ?? false)
    }).catch(e => { if (active) setError(e.message) }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [sessionId])
  async function save(remove = false) {
    setBusy(true); setError(""); setMessage("")
    try {
      if (remove) await api.deleteWebhook(sessionId)
      else await api.putWebhook(sessionId, { url, filter, batch_size: Number(size), flush_interval_seconds: Number(interval), ...(secret ? { signing_secret: secret } : {}) })
      if (remove) { setUrl(""); setFilter(""); setHasSecret(false) }
      else if (secret) setHasSecret(true)
      setSecret("")
      setMessage(remove ? "New exports disabled. Accepted events will continue retrying until delivered." : "Webhook saved. Delivery begins when the session configuration is applied.")
    } catch (e) { setError(e instanceof Error ? e.message : String(e)) }
    finally { setBusy(false) }
  }
  if (loading) return <>Loading the webhook</>
  return <SpaceBetween size="m">
    <p key="description">Send tool-call events with durable at-least-once delivery, including browser and shell attempts captured before authorization. Failed deliveries retry until acknowledged. Receivers must deduplicate event IDs. Calls are refused if their event cannot be durably recorded.</p>
    {error && <Alert key="error" type="error">{error}</Alert>}
    {message && <Alert key="message" type="success">{message}</Alert>}
    <FormField key="url" label="Webhook URL" description="A public HTTPS endpoint on port 443."><Input value={url} onChange={e => setUrl(e.detail.value)} /></FormField>
    <FormField key="batch-size" label="Batch size" description="1 to 500 events."><Input type="number" value={size} onChange={e => setSize(e.detail.value)} /></FormField>
    <FormField key="interval" label="Flush interval (seconds)" description="1 to 60 seconds for a partial batch."><Input type="number" value={interval} onChange={e => setInterval(e.detail.value)} /></FormField>
    <FormField key="secret" label="Signing secret" description={hasSecret ? "A secret is configured. Leave blank to keep it, or enter a replacement." : "Optional, 16 to 256 bytes. Signs each delivery with HMAC-SHA256."}><Input type="password" value={secret} onChange={e => setSecret(e.detail.value)} /></FormField>
    <FormField key="filter" label="Rego filter (optional)" description="Leave empty to send every event. Define package browserjs.policy and allow_tool_call; input is the event, and only true includes it."><Textarea rows={12} value={filter} onChange={e => setFilter(e.detail.value)} placeholder={'package browserjs.policy\nimport rego.v1\n\nallow_tool_call if input.tool == "exec"'} /></FormField>
    <SpaceBetween key="actions" direction="horizontal" size="s"><Button key="save" variant="primary" loading={busy} disabled={!url} onClick={() => void save()}>Save webhook</Button><Button key="disable" disabled={busy || !url} onClick={() => void save(true)}>Disable new exports</Button></SpaceBetween>
  </SpaceBetween>
}
