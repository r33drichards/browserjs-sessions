# Your first run_js program

In this tutorial you write the kind of program an agent writes, and run it
against your desktop. You need a session connected to an MCP client; see
[Create a desktop and connect an agent](/tutorials/first-desktop).

To run each program, ask your agent to call `run_js` with exactly that code,
or call the tool yourself if your client lets you.

Keep the session's page open to watch.

## 1. Print something

```js
console.log(1 + 1)
```

The result is `2`. A program returns what it prints, and nothing else.

## 2. Drive the browser

```js
const r = await mcp.callTool("browser", "browser_execute", {
  operations: [
    { type: "navigate", params: { url: "https://example.com" } },
    { type: "evaluate", params: { script: "document.querySelector('h1').innerText" } },
  ],
})
console.log(r.content[0].text)
```

Chromium opens the page. The program prints the result of each step, as
JSON; the heading is in the second. Two steps took one tool call.

## 3. Decide in code

```js
// The result text is a line of summary, then the steps' results as JSON.
const call = async ops => {
  const r = await mcp.callTool("browser", "browser_execute", { operations: ops })
  const text = r.content[0].text
  return JSON.parse(text.slice(text.indexOf("[")))
}

const [links] = await call([{ type: "evaluate", params: { script: "document.links.length" } }])
console.log(links.result.result)
```

The program prints one number, not the whole result: it returns only what
matters. The page stayed open from step 2: the desktop keeps its state
between calls, even though the program's variables do not.

From here an agent adds loops and conditions: read a list, visit each item,
keep only the rows that match, print a short table.

## 4. Use the whole desktop

`browser_execute` works inside web pages. `desktop_execute` uses the mouse
and keyboard on the screen, like a person.

```js
const r = await mcp.callTool("browser", "desktop_execute", {
  operations: [
    { type: "keyboard.pressKey", params: { keys: ["LeftControl", "L"] } },
    { type: "keyboard.releaseKey", params: { keys: ["LeftControl", "L"] } },
    { type: "keyboard.type", params: { text: "example.org" } },
    { type: "keyboard.type", params: { keys: ["Enter"] } },
    { type: "sleep", params: { ms: 2000 } },
    { type: "screen.grab" },
  ],
})
const { results, screen } = JSON.parse(r.content[0].text)
console.log(JSON.stringify(screen))
```

On the screen, the address bar is selected, text is typed, and the page
loads. The program prints the screen's size.

The screenshot came back to the program, not to the model. To look at it,
attach it:

```js
const r = await mcp.callTool("browser", "desktop_execute", { operations: [{ type: "screen.grab" }] })
const { results } = JSON.parse(r.content[0].text)
const png = r.content[1 + results[0].result.image_index]
artifact("screen", "image/png", Uint8Array.from(atob(png.data), c => c.charCodeAt(0)))
```

## 5. Remember something

```js
await fs.mkdir("/data/memory", { recursive: true })
await fs.writeFile("/data/memory/INDEX.md", "- notes.md — what I learned in the tutorial\n")
await fs.writeFile("/data/memory/notes.md", "example.com has one link.\n")
console.log(await fs.readFile("/data/memory/notes.md", "utf8"))
```

This is on the session's disk. It is there after sleep, and for the next
agent.

## What you have seen

One tool. Programs that take several steps and return a line of text. A
desktop that keeps its state between calls. A disk for what the agent
learns.

## What next

- [Capabilities: browser, desktop, shell](/reference/capabilities)
- [Why code mode](/explanation/code-mode)
