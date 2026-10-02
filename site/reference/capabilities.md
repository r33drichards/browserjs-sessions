# Capabilities: browser, desktop, shell

What code in `run_js` can call, and what is on the desktop for it to act on.

| Capability | Call | Status |
| --- | --- | --- |
| Browser | `mcp.callTool("browser", "browser_execute", …)` | Live |
| Desktop | `mcp.callTool("browser", "desktop_execute", …)` | Live |
| Shell | `mcp.callTool("exec", "exec", …)` | Live |

Use the browser capability for anything inside a web page: it finds elements
by selector and does not depend on where things are on screen. Use the
desktop capability for what a page does not contain: the browser's own
interface, dialogs such as the file chooser, other windows.

## The desktop itself

| Part | Today |
| --- | --- |
| Display | One X display. 1280 by 800 at first; it follows the viewer's window, from 320 by 200 to 2560 by 1600 |
| Desktop | XFCE: a panel along the bottom with a menu, launchers and the open windows |
| Applications | Chromium, a terminal, a file manager (Thunar), a text editor (Mousepad), an image viewer |
| Fonts | DejaVu, Noto, colour emoji |
| Network | The public internet |

A session starts with the desktop empty. When it is created, Chromium starts
in the background without a window, so the first `browser_execute` call
finds it ready; that call, the launcher on the panel or a link opened from
another program opens its window. Its window opens
maximised. It is one profile, kept on the session's disk, however it was
started. If you close Chromium it stays closed, and the next
`browser_execute` call starts it again with its tabs.

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

Runs programs on the desktop, as the desktop's user, in its home directory
and with its files. A program can open a window on the screen. The calls go to a second server, `"exec"`, and are asynchronous:
`exec` starts a program and returns at once, the other three follow it.

```js
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
const { id } = await call("exec", { bin: "ls", args: ["-la", "/data/chrome/Downloads"], timeout: 60 });
let logs = "", offset = 0, status = "running";
while (status === "running") {
  const r = await call("stream_logs", { id, offset });
  logs += r.logs; offset = r.next_offset; status = r.status;
  if (status === "running") await new Promise((resolve) => setTimeout(resolve, 250));
}
console.log(status, logs); // "completed:0" and the listing
```

| Tool | Arguments | Returns |
| --- | --- | --- |
| `exec` | `bin`, `args?`, `timeout`, `cwd?`, `env?` | `{ id, status: "started" }` |
| `stream_logs` | `id`, `offset` | `{ logs, next_offset, status }`: the output from that byte on |
| `search_logs` | `id`, `pattern` | `{ matches: [{ line, offset }] }` for a regular expression |
| `kill` | `id` | `{ id, status }`; stops the command and everything it started |

| `exec` argument | Type | Meaning |
| --- | --- | --- |
| `bin` | string, required | The program: a name found on `PATH`, or a path |
| `args` | array of strings, optional | Its arguments, each passed exactly as written |
| `timeout` | integer, required | Seconds. Then the command and everything it started are killed |
| `cwd` | string, optional | Working directory, an absolute path. Default: the home directory |
| `env` | object of strings, optional | Variables added to the desktop's environment |

`status` is `running`, `completed:<exit code>`, `timeout`, `cancelled` or
`failed:<reason>`.

How it behaves:

- **No shell.** The program is run directly. Nothing in `args` is split,
  expanded or interpreted: `"*.csv"` and `"$HOME"` arrive as those
  characters. For pipes, redirection and globs, run a shell as the program:
  `{ bin: "sh", args: ["-c", "ls *.csv | wc -l"], timeout: 10 }`.
- **Nothing else is accepted.** A call with a field not in the table, such
  as a command line in `cmd`, is refused.
- **Output.** stdout and stderr share one log, line by line. It is kept on
  the session's disk for 7 days and survives sleep. A command that was
  running when the session slept reads `failed:interrupted` afterwards.
- **Exit codes.** A program that fails is a result, not an error of the
  call. Check for `completed:0`.
- **No terminal.** Programs get no keyboard input and cannot prompt.
- **What is installed.** `bash` and the standard Unix tools (`ls`, `cat`,
  `cp`, `sed`, `find`, `ps`). No `git`, `curl` or language runtimes yet; they come with the planned XFCE desktop. There
  is no `sudo` and no way to install system packages.
- **Limits.** Commands run as an unprivileged user inside the session's
  sandbox and reach the network the browser reaches.
