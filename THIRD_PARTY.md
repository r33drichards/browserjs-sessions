# Third-party software

The source in this repository is under the [Apache License 2.0](LICENSE). It
contains build recipes (Dockerfiles, a Nix flake, lock files) for images that
bundle other software; that software is not in this repository and keeps its
own licence. If you publish the images you build, you distribute those
programs and take on their terms. Versions are fixed by `images/*/flake.lock`,
the image digests in the Dockerfiles, and the lock files.

| Component | Where | Licence | What it means here |
|---|---|---|---|
| Chromium | `browser` image (nixpkgs) | BSD-3-Clause and others | redistributable with its notices |
| TigerVNC (`Xvnc` only) | `browser` image | GPL-2.0-or-later | a separate program in the image; whoever distributes the image must offer its source (nixpkgs pins it) |
| openbox | `browser` image | GPL-2.0-or-later | as TigerVNC |
| noVNC | `browser` image; `@novnc/novnc` in `web/` | MPL-2.0 (some files BSD, MIT, OFL) | used unmodified; changes to its files would have to be published |
| websockify | `browser` image | LGPL-3.0 | run as a separate program |
| nut.js fork (`@nut-tree-fork/nut-js`, libnut) | `browser` image | Apache-2.0 | |
| puppeteer-core | `browser` image | Apache-2.0 | |
| Caddy | `browser` image | Apache-2.0 | |
| mcp-exec | `browser` image | see its repository | by the same author as this repository |
| mcp-js | base of the `mcp-js` image | AGPL-3.0 | run unmodified as a separate service; this repository adds configuration only. Whoever runs a modified mcp-js for others over a network must offer them its source |
| Open Policy Agent | `policy-operator` image | Apache-2.0 | |
| kopf and the Python dependencies | the two operator images (`requirements.txt`) | MIT, BSD, Apache-2.0 | |
| Python, Debian (`python:3.12-slim`), distroless | image bases | PSF, various | |
| Cloudscape, React, Monaco | `web/` (npm) | Apache-2.0, MIT, MIT | bundled into the backend image |
| Go modules | `backend/`, `terraform-provider-browserjs/` (`go.sum`) | BSD, MIT, Apache-2.0, MPL-2.0 (HashiCorp plugin framework) | |
| Fonts: DejaVu, Noto | `browser` image | Bitstream Vera terms, OFL-1.1 | |
| Pomerium, Dex, cert-manager, Agent Sandbox | pulled by `deploy/`, not built here | Apache-2.0 | |

This table is a summary made by reading each project's licence file, not a
legal opinion and not a generated bill of materials.
