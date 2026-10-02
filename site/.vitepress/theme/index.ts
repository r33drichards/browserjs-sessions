import type { Theme } from "vitepress"
// No bundled web font: system fonts only.
import DefaultTheme from "vitepress/theme-without-fonts"
import BlogIndex from "./BlogIndex.vue"
import "./custom.css"

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component("BlogIndex", BlogIndex)
  },
} satisfies Theme
