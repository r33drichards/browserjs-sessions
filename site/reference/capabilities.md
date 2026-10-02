# Capabilities: browser, desktop, shell

What code in `run_js` can call, and what is on the desktop for it to act on.

| Capability | Call | Status |
| --- | --- | --- |
| Browser | `mcp.callTool("browser", "browser_execute", …)` | Live |
| Desktop | `mcp.callTool("browser", "desktop_execute", …)` | Live |
| Shell | `exec` | Planned |

Use the browser capability for anything inside a web page: it finds elements
by selector and does not depend on where things are on screen. Use the
desktop capability for what a page does not contain: the browser's own
interface, dialogs such as the file chooser, other windows.

## The desktop itself

| Part | Today |
| --- | --- |
| Display | One X display. 1280 by 800 at first; it follows the viewer's window, from 320 by 200 to 2560 by 1600 |
| Window manager | openbox. Ordinary windows are maximised |
| Applications | Chromium, always open. It restarts if closed |
| Fonts | DejaVu, Noto, colour emoji |
| Network | The public internet |

::: info Planned
An XFCE desktop with a terminal and a file manager. Until it lands, Chromium
is the only application and there is no terminal.
:::

## Browser: `browser_execute`

Runs a list of operations in one tab of the desktop's Chromium.

| Parameter | Type | Meaning |
| --- | --- | --- |
| `operations` | array, required | Steps, each `{ type, params }`, run in order |
| `tab` | string, optional | Name of the tab. Default `"default"`. Created on first use, reused by later calls. Calls on one tab run one at a time |
| `close` | boolean, optional | Close the tab afterwards. Default: leave it open |

| Operation | `params` | Notes |
| --- | --- | --- |
| `navigate` | `{ url, waitUntil? }` | Waits for `domcontentloaded` by default. 45 second timeout |
| `wait` | `{ ms }` or `{ selector, ms? }` | A fixed time, or a visible element (10 seconds unless `ms` is given). At most 30 seconds |
| `click` | `{ selector }` | Waits up to 10 seconds for the element |
| `type` | `{ selector, text, delay? }` | Clicks the element, then types |
| `press` | `{ key }` | For example `"Enter"` |
| `select` | `{ selector, values }` | Options of a `<select>` |
| `evaluate` | `{ script }` | Runs in the page; returns the result |
| `screenshot` | `{ fullPage? }` | A PNG of the page |
| `url` | `{}` | Current URL and title |
| `setViewport` | `{ width, height }` | 320 by 200 up to 3840 by 2160 |
| `setContent` | `{ html }` | Replaces the page |

The result's text is a summary line followed by the steps' results as JSON.
Steps stop at the first failure, and a screenshot of that moment is
attached. A tab stays open between calls.

## Desktop: `desktop_execute`

Uses the mouse, keyboard, screen and clipboard of the display, as a person
at the screen would.

| Parameter | Type | Meaning |
| --- | --- | --- |
| `operations` | array, required | Steps, each `{ type, params }`, run in order |
| `config` | object, optional | `keyboardDelayMs` (default 10), `mouseDelayMs` (default 50), `mouseSpeed` (default 2000 px/s) |

| Group | Operations |
| --- | --- |
| Mouse | `mouse.setPosition {x, y}`, `mouse.move {x, y}`, `mouse.getPosition`, `mouse.click {button?, x?, y?}`, `mouse.doubleClick {button?, x?, y?}`, `mouse.pressButton {button?}`, `mouse.releaseButton {button?}`, `mouse.drag {to, from?}`, `mouse.scrollUp`, `mouse.scrollDown`, `mouse.scrollLeft`, `mouse.scrollRight` `{amount, x?, y?}` |
| Keyboard | `keyboard.type {text}`, `keyboard.type {keys}`, `keyboard.pressKey {keys}`, `keyboard.releaseKey {keys}` |
| Screen | `screen.width`, `screen.height`, `screen.grab`, `screen.grabRegion {left, top, width, height}`, `screen.colorAt {x, y}` |
| Windows | `getActiveWindow`, `getWindows` |
| Clipboard | `clipboard.setContent {text}`, `clipboard.getContent` |
| Other | `sleep {ms}`, up to 30000 |

Buttons are `LEFT` (default), `MIDDLE`, `RIGHT`. Keys use nut.js names, such
as `Enter`, `Escape`, `Tab`, `LeftControl`, `LeftShift`, `LeftAlt`, `A` to
`Z`, `Num0` to `Num9`, `F1` to `F24`. The tool's own description lists them
all.

How it behaves:

- **Results.** The text is JSON: `{ results: [{ success, operation, result }], screen: { width, height } }`. Screenshots are PNG images after the text; a screenshot's `result` is `{ image_index, width, height }`.
- **Screenshots go to the code, not to the model.** Attach one with `artifact(...)` only when the model needs to see it. For a large one, pass `heap_memory_max_mb: 64` to `run_js`.
- **Coordinates** are screen pixels from the top left. The screen changes size when a viewer resizes their window, so read `screen` before aiming.
- **Failures.** The whole call is checked before anything runs. It stops at the first failing step and attaches a screenshot. Keys and buttons still held are released.
- **Text.** `keyboard.type {text}` presses the keys of a US layout. For other characters, put the text on the clipboard and paste it.
- **One call at a time.** The person watching sees the pointer move and can use the mouse and keyboard too.

## Shell: `exec`

::: info Planned
Running commands on the desktop container from `run_js`. Not available: no
call exists yet, and the container has no shell tools for it to run.
:::
