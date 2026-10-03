// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { api } from "../api"
import { WebhookTab } from "./WebhookTab"

afterEach(() => { cleanup(); vi.restoreAllMocks() })

describe("session webhook", () => {
  it("preserves a saved signing secret and sends an optional filter", async () => {
    vi.spyOn(api, "getWebhook").mockResolvedValue({ url: "https://example.com/hook", has_signing_secret: true })
    const put = vi.spyOn(api, "putWebhook").mockResolvedValue(undefined)
    render(<WebhookTab sessionId="s-abcdefghij" />)
    await screen.findByText(/A secret is configured/)
    fireEvent.change(screen.getByLabelText("Rego filter (optional)"), { target: { value: "package browserjs.policy\nallow_tool_call := true" } })
    fireEvent.click(screen.getByRole("button", { name: "Save webhook" }))
    await waitFor(() => expect(put).toHaveBeenCalledWith("s-abcdefghij", { url: "https://example.com/hook", filter: "package browserjs.policy\nallow_tool_call := true", batch_size: 100, flush_interval_seconds: 5 }))
    await screen.findByText(/Webhook saved/)
  })

  it("keeps edits when validation fails and can disable exports", async () => {
    vi.spyOn(api, "getWebhook").mockResolvedValue({ url: "https://example.com/hook" })
    vi.spyOn(api, "putWebhook").mockRejectedValue(new Error("the webhook filter does not validate"))
    const remove = vi.spyOn(api, "deleteWebhook").mockResolvedValue(undefined)
    render(<WebhookTab sessionId="s-abcdefghij" />)
    await screen.findByRole("button", { name: "Save webhook" })
    fireEvent.click(screen.getByRole("button", { name: "Save webhook" }))
    await screen.findByText("the webhook filter does not validate")
    expect((screen.getByLabelText("Webhook URL") as HTMLInputElement).value).toBe("https://example.com/hook")
    fireEvent.click(screen.getByRole("button", { name: "Disable webhook" }))
    await screen.findByText("Webhook disabled.")
    expect(remove).toHaveBeenCalledWith("s-abcdefghij")
  })
})
