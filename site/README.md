# site

The public site for computeruse: landing page, docs and blog, served at https://computeruse.site. One VitePress
site. The internal engineering docs stay in `/docs` and are not published.

```bash
npm install
npm run dev      # http://localhost:5173
npm run build    # static files in .vitepress/dist
npm run preview  # serve the build
```

Node 20 or newer. With nix: `nix shell nixpkgs#nodejs_22 -c npm run dev`.

## Layout

- `index.md`: the landing page.
- `tutorials/`, `guides/`, `reference/`, `explanation/`: the docs, by
  [Diátaxis](https://diataxis.fr) section. A new page also goes in the
  sidebar in `.vitepress/config.ts`.
- `blog/`: one markdown file per post, with `title`, `date` and `description`
  frontmatter. The index lists them newest first (`blog/posts.data.ts`).
- `.vitepress/theme/custom.css`: the wireframe look.

The build uses clean URLs (`/guides/files`, no `.html`), so the host has to
serve `/guides/files.html` for `/guides/files`.

## Deployment

Merging a change under `site/` into `main` builds and publishes the site image,
then automatically deploys that digest through the site's Argo canary. Pull
requests build only. The rest of the cluster retains its manual release
flow. Retry from Actions by running **images** on `main` with `images=site`.
See [`docs/build-pipeline.md`](../docs/build-pipeline.md#automatic-site-releases)
for verification and rollback behavior.
