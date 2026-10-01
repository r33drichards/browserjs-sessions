// Keycloak singleton, initialised once by AuthProvider. Settings come from
// window.__BROWSERJS_CFG__, served by the backend at /config.js, so one build
// works in every environment.
import Keycloak from "keycloak-js"

declare global {
  interface Window {
    __BROWSERJS_CFG__?: { kcUrl?: string; kcRealm?: string; kcClientId?: string }
  }
}

const cfg = window.__BROWSERJS_CFG__ ?? {}

export const kc = new Keycloak({
  url: cfg.kcUrl ?? "http://localhost:8081",
  realm: cfg.kcRealm ?? "browserjs",
  clientId: cfg.kcClientId ?? "browserjs-spa",
})

let initialisation: Promise<boolean> | null = null

export function initKc(): Promise<boolean> {
  if (initialisation) return initialisation
  initialisation = kc
    .init({ onLoad: "login-required", flow: "standard", pkceMethod: "S256", checkLoginIframe: false })
    .then(authed => {
      // onTokenExpired fires after expiry; refresh ahead of it instead.
      kc.onTokenExpired = () => {
        kc.updateToken(30).catch(() => kc.login())
      }
      return authed
    })
    .catch(error => {
      initialisation = null
      throw error
    })
  return initialisation
}

// A fresh access token, refreshed if it is within 30s of expiry.
export async function getToken(): Promise<string | undefined> {
  if (!kc.authenticated) return undefined
  try {
    await kc.updateToken(30)
  } catch {
    await kc.login()
    return undefined
  }
  return kc.token
}

export const isAdmin = () => kc.hasRealmRole("admin")
export const username = () => (kc.tokenParsed?.preferred_username as string | undefined) ?? ""
