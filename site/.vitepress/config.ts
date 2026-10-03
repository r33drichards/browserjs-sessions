import { defineConfig } from "vitepress"

// Where the site is served.
const origin = "https://computeruse.site"
const app = "https://app.computeruse.site"

const tutorials = [
  { text: "Create a desktop and connect an agent", link: "/tutorials/first-desktop" },
  { text: "Your first run_js program", link: "/tutorials/first-program" },
]

const guides = [
  { text: "Sleep, wake, stop and start", link: "/guides/sleep-wake-resume" },
  { text: "Watch and take over", link: "/guides/take-over" },
  { text: "Move files and the clipboard", link: "/guides/files-and-clipboard" },
  { text: "Write a policy", link: "/guides/write-a-policy" },
  { text: "Export tool calls", link: "/guides/tool-call-webhooks" },
  { text: "Use it from code", link: "/guides/use-from-code" },
]

const reference = [
  { text: "Session lifecycle", link: "/reference/lifecycle" },
  { text: "Session sizes", link: "/reference/session-sizes" },
  { text: "MCP endpoint and run_js", link: "/reference/mcp" },
  { text: "Capabilities: browser, desktop, shell", link: "/reference/capabilities" },
  { text: "Policy format", link: "/reference/policy" },
  { text: "Limits", link: "/reference/limits" },
  { text: "HTTP API", link: "/reference/api" },
  { text: "SDK: Rust, Python, JavaScript, Go", link: "/reference/sdk" },
  { text: "Live, coming and planned", link: "/reference/status" },
]

const explanation = [
  { text: "Serverless, resumable desktop containers", link: "/explanation/desktop-containers" },
  { text: "Why code mode", link: "/explanation/code-mode" },
  { text: "The containment model", link: "/explanation/containment" },
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
  description: "Serverless, resumable desktop containers with a code-mode MCP tool, a light admin interface, and policy to contain what agents do.",
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
