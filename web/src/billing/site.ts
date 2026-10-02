// Addresses on the public site (the legal pages, pricing) and the support
// address: served by the backend in /config.js where they differ (the
// declaration is in shell.tsx).
export const siteUrl = (path: string) => (window.__BROWSERJS_CFG__?.siteUrl ?? "https://computeruse.site").replace(/\/$/, "") + path

export const supportEmail = () => window.__BROWSERJS_CFG__?.supportEmail ?? "browserjs06@gmail.com"
