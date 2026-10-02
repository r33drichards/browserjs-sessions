import { defineConfig } from "vitepress"

const tutorials = [
  { text: "Your first session", link: "/tutorials/first-session" },
  { text: "Connect a session to Claude", link: "/tutorials/connect-claude" },
]

const guides = [
  { text: "Create, stop, resume and delete", link: "/guides/manage-sessions" },
  { text: "Copy and paste", link: "/guides/copy-paste" },
  { text: "Move files in and out", link: "/guides/files" },
  { text: "Full screen and resize", link: "/guides/full-screen" },
  { text: "Wake a sleeping session", link: "/guides/wake" },
  { text: "Connect another MCP client", link: "/guides/mcp-clients" },
]

const reference = [
  { text: "Session states", link: "/reference/session-states" },
  { text: "MCP endpoint and tools", link: "/reference/mcp" },
  { text: "Limits", link: "/reference/limits" },
  { text: "HTTP API", link: "/reference/api" },
]

const explanation = [
  { text: "Sleep and wake", link: "/explanation/sleep-and-wake" },
  { text: "What persists", link: "/explanation/persistence" },
  { text: "Security model", link: "/explanation/security" },
]

// Every docs page shows all four sections.
const docs = [
  { text: "Tutorials", items: tutorials },
  { text: "How-to guides", items: guides },
  { text: "Reference", items: reference },
  { text: "Explanation", items: explanation },
]

export default defineConfig({
  title: "browserjs",
  description: "Your own persistent cloud browser, with an MCP endpoint for your agents.",
  lang: "en-US",
  cleanUrls: true,
  srcExclude: ["README.md"],
  lastUpdated: false,
  // Black on white only, like the app.
  appearance: false,
  themeConfig: {
    nav: [
      { text: "Tutorials", link: tutorials[0].link, activeMatch: "^/tutorials/" },
      { text: "How-to", link: guides[0].link, activeMatch: "^/guides/" },
      { text: "Reference", link: reference[0].link, activeMatch: "^/reference/" },
      { text: "Explanation", link: explanation[0].link, activeMatch: "^/explanation/" },
      { text: "Blog", link: "/blog/", activeMatch: "^/blog/" },
      { text: "Open the app", link: "https://app.browserjs.com" },
    ],
    sidebar: {
      "/tutorials/": docs,
      "/guides/": docs,
      "/reference/": docs,
      "/explanation/": docs,
    },
    search: { provider: "local" },
    outline: [2, 3],
  },
})
