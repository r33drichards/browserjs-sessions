# Why code mode

A session gives an agent one tool, `run_js`. The agent does not get a tool
for clicking, one for typing and one for reading. It writes a program that
does those things. This is called code mode.

## The usual way

The usual way to give a model a computer is a set of tools: `click`, `type`,
`screenshot`. Each step is a round trip. The model asks for a click, waits
for the result, reads it, and asks for the next step.

For a task of thirty steps that is thirty round trips. Each one adds its
result to the model's context, including results the model did not need.
Each one takes seconds. And the model has to carry the plan in its head from
step to step.

## The other way

Models write code well. So let the model write the thirty steps as a
program:

```js
const call = async ops => {
  const r = await mcp.callTool("browser", "browser_execute", { operations: ops })
  const t = r.content[0].text
  return JSON.parse(t.slice(t.indexOf("[")))
}

const rows = []
for (const id of ["a1", "a2", "a3"]) {
  const [, price] = await call([
    { type: "navigate", params: { url: `https://shop.example/item/${id}` } },
    { type: "evaluate", params: { script: "document.querySelector('.price').innerText" } },
  ])
  rows.push(`${id}: ${price.result.result}`)
}
console.log(rows.join("\n"))
```

The program runs next to the desktop. The model gets back three lines.

## What changes

- **Fewer round trips.** Steps that do not need the model's judgment run
  one after another without it.
- **Less context.** The program decides what to print. A page of HTML or a
  screenshot stays with the code unless the model asks to see it.
- **Real control flow.** Loops, conditions, retries and waits are written
  down, not improvised step by step.
- **One tool to learn.** Adding a capability to the desktop adds something
  code can call. It does not add another tool to the model's list.

## What it costs

- The model must write correct code. When a program fails, the error and a
  screenshot of that moment come back, and the model writes the next one.
- A program runs to its end or to its time limit, 30 seconds by default and
  at most 300. Long tasks are several programs, not one.
- Each program starts fresh. What should last between programs is on the
  desktop, or written to `/data/memory/`.

## Step by step is still possible

Nothing forces long programs. A program can be one click. An agent that is
unsure looks first, with a short program that returns a screenshot, then
acts. Code mode makes many steps cheap; it does not forbid single ones.

## Where policy fits

Every call a program makes to the desktop passes one checkpoint, whatever
the program around it does. That checkpoint is where a session's policy
decides. See [The containment model](/explanation/containment).
