import { createContentLoader } from "vitepress"

export interface Post {
  title: string
  description: string
  url: string
  date: string // ISO 8601
  day: string // as shown, e.g. "1 October 2026"
}

declare const data: Post[]
export { data }

export default createContentLoader("blog/*.md", {
  transform(pages): Post[] {
    return pages
      .filter(page => page.url !== "/blog/")
      .map(page => {
        const date = new Date(page.frontmatter.date)
        return {
          title: page.frontmatter.title,
          description: page.frontmatter.description ?? "",
          url: page.url,
          date: date.toISOString(),
          // Frontmatter dates are UTC midnight; show the day that was written.
          day: date.toLocaleDateString("en-GB", { day: "numeric", month: "long", year: "numeric", timeZone: "UTC" }),
        }
      })
      .sort((a, b) => b.date.localeCompare(a.date))
  },
})
