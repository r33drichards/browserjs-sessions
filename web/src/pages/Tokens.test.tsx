// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"
import { renderAt, startBackend } from "../test/harness"
import { CreateToken, tokenRequest } from "./CreateToken"
import { Tokens } from "./Tokens"

afterEach(cleanup)

const routes = [
  { path: "/tokens", element: <Tokens /> },
  { path: "/tokens/create", element: <CreateToken /> },
]

describe("tokens", () => {
  it("creates a token, shows its secret once with the warning, then lists it without", async () => {
    const { writes } = startBackend()
    const { router } = renderAt("/tokens/create", routes)
    fireEvent.change(await screen.findByPlaceholderText("terraform-ci"), { target: { value: "ci" } })
    fireEvent.click(screen.getByRole("checkbox", { name: /policies:write/ }))
    fireEvent.click(screen.getByRole("checkbox", { name: /sessions:read/ }))
    fireEvent.click(screen.getByRole("button", { name: "Create token" }))

    const secret = (await screen.findByTestId("token-secret")).textContent!
    expect(secret).toMatch(/^bjs_[a-z0-9]+_/)
    expect(writes()[0]).toMatchObject({ path: "/api/tokens", body: { name: "ci", scopes: ["sessions:read", "policies:write"], expires_in_days: 90 } })
    expect(screen.getByText("Copy the token now. It is not shown again.")).toBeTruthy()
    expect(screen.getByText(/hands the agent the policy/)).toBeTruthy()

    // Leaving without having copied it loses it for good: ask.
    fireEvent.click(screen.getByRole("button", { name: "Done" }))
    fireEvent.click(await screen.findByRole("button", { name: "Leave" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/tokens"))
    expect(await screen.findByText("ci")).toBeTruthy()
    expect(document.body.textContent).not.toContain(secret)

    fireEvent.click(screen.getByRole("button", { name: "Revoke ci" }))
    fireEvent.click(screen.getByRole("button", { name: "Revoke token" }))
    expect(await screen.findByText(/No tokens\./)).toBeTruthy()
    expect(writes().at(-1)).toMatchObject({ method: "DELETE" })
  })

  it("says so where the deployment has no tokens, without an error", async () => {
    startBackend({ tokens: false })
    renderAt("/tokens", routes)
    expect(await screen.findByText("API tokens aren't available on this deployment.")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Create token" })).toBeNull()
  })

  it("validates the form before asking", () => {
    expect(tokenRequest({ name: " ", scopes: [], days: "0" }).errors).toEqual({
      name: "Enter a name.",
      scopes: "Choose at least one scope.",
      days: "Enter a number of days from 1 to 365.",
    })
    expect(tokenRequest({ name: "x".repeat(65), scopes: ["sessions:read"], days: "366" }).errors).toHaveProperty("name")
    expect(tokenRequest({ name: " ci ", scopes: ["policies:write", "sessions:read"], days: "30" })).toEqual({
      body: { name: "ci", scopes: ["sessions:read", "policies:write"], expires_in_days: 30 },
      errors: {},
    })
  })
})
