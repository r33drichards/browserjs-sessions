import { defineConfig } from "vitepress"

// Where the site is served.
const origin = "https://computeruse.site"
const app = "https://app.computeruse.site"

const tutorials = [
  { text: "Your first desktop", link: "/tutorials/first-session" },
  { text: "Connect an agent with Claude", link: "/tutorials/connect-claude" },
]

const guides = [
  { text: "Create, stop, resume and delete", link: "/guides/manage-sessions" },
  { text: "Watch an agent and take over", link: "/guides/take-over" },
  { text: "Copy and paste", link: "/guides/copy-paste" },
  { text: "Move files in and out", link: "/guides/files" },
  { text: "Full screen and resize", link: "/guides/full-screen" },
  { text: "Wake a sleeping session", link: "/guides/wake" },
  { text: "Connect another MCP client", link: "/guides/mcp-clients" },
]

const reference = [
  { text: "What is on the desktop", link: "/reference/desktop" },
  { text: "Session states", link: "/reference/session-states" },
  { text: "MCP endpoint and tools", link: "/reference/mcp" },
  { text: "Limits", link: "/reference/limits" },
  { text: "HTTP API", link: "/reference/api" },
  { text: "Live, coming and planned", link: "/reference/status" },
]

const explanation = [
  { text: "Computer use on a Linux desktop", link: "/explanation/computer-use" },
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
  title: "Computer Use",
  description: "A small Linux desktop in the cloud that an AI agent operates over MCP, and that you can watch and take over.",
  lang: "en-US",
  cleanUrls: true,
  srcExclude: ["README.md"],
  lastUpdated: false,
  // Black on white only, like the app.
  appearance: false,
  sitemap: { hostname: origin },
  // Each page names its one address.
  transformPageData(pageData) {
    const path = pageData.relativePath.replace(/(^|\/)index\.md$/, "$1").replace(/\.md$/, "")
    pageData.frontmatter.head ??= []
    pageData.frontmatter.head.push(["link", { rel: "canonical", href: `${origin}/${path}` }])
  },
  themeConfig: {
    nav: [
      { text: "Tutorials", link: tutorials[0].link, activeMatch: "^/tutorials/" },
      { text: "How-to", link: guides[0].link, activeMatch: "^/guides/" },
      { text: "Reference", link: reference[0].link, activeMatch: "^/reference/" },
      { text: "Explanation", link: explanation[0].link, activeMatch: "^/explanation/" },
      { text: "Blog", link: "/blog/", activeMatch: "^/blog/" },
      { text: "Open the app", link: app },
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
