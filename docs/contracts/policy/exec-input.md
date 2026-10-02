# The `exec` server (mcp-exec): what a policy sees

For someone writing a session policy in Rego (`rego-contract.md`: package
`browserjs.policy`, entry rule `allow_tool_call`) that allows, denies or
constrains shell commands.

Commands are run by [mcp-exec](https://github.com/r33drichards/mcp-exec),
which mcp-js knows as the upstream server `"exec"`
(`images/mcp-js/mcp-servers.json`). It runs inside the browser container
(`images/browser/browser/exec-server.sh`), so a command runs on the desktop:
as its user, in its home directory, with its `PATH` and `DISPLAY`.

Machine-readable: [`tools/exec.schema.json`](tools/exec.schema.json),
[`tools/stream_logs.schema.json`](tools/stream_logs.schema.json),
[`tools/search_logs.schema.json`](tools/search_logs.schema.json), and a sample
input for each next to them. Policies and their cases:
[`tools/examples/`](tools/examples/).

## The input

Agent code in `run_js` calls

```js
await mcp.callTool("exec", "exec", { cmd: "cd /home/browser/work && git status", timeout: 60 });
```

and mcp-js v0.21.0-rc.3 asks the policy before it forwards the call, with
(`server/src/engine/mcp_client.rs`, `McpToolPolicyInput`):

```json
{
  "operation": "mcp_call_tool",
  "server": "exec",
  "tool": "exec",
  "arguments": {
    "cmd": "cd /home/browser/work && git status",
    "timeout": 60
  }
}
```

`arguments` is the object exactly as the agent's code passed it, or `null`
when it passed none.

| Tool | `arguments` (all required) | What it does |
|---|---|---|
| `exec` | `cmd`: string; `timeout`: integer, **seconds** | Runs `sh -c <cmd>` and answers at once with `{id, status: "started"}`. The command goes on running. |
| `stream_logs` | `id`: string (the UUID); `offset`: integer | The command's output from that byte on, where to continue, and its status (`running`, `completed:<exit code>`, `timeout`, `failed:…`, `cancelled`). |
| `search_logs` | `id`: string; `pattern`: string | The lines of the output matching a regular expression. |

These are the fields of `ExecRequest`, `StreamLogsRequest` and
`SearchLogsRequest` in mcp-exec's `src/service.rs` at the commit the image
pins (`images/browser/flake.lock`); the image build compares them with what
the packaged server lists (`images/browser/test/exec-smoke.mjs`).

**The policy is asked before the server parses anything.** Every field can be
missing or of any type, and other fields can be present: mcp-exec ignores
fields it does not know and refuses a wrong type. Test types (`is_string`,
`is_number`) before matching.

`stream_logs` and `search_logs` start nothing. A policy that allows some
commands can allow them without looking at their arguments; one that allows
no commands can deny them with everything else. There is no tool that lists
commands, and none that stops one.

## What a policy can decide from `cmd`

`cmd` is the whole command: one string, handed to a shell. There is **no
program-and-arguments form, no working directory field and no environment
field**; a directory or a variable is set by writing `cd dir && …` or
`VAR=value …` inside the string. So a policy on `exec` is a policy on a
string, and what it can establish depends on how much of the shell's language
it lets through:

1. **The exact strings on a list.** `input.arguments.cmd in allowed_commands`.
   Nothing to reason about; right whenever the commands are known in advance.
   ([`exec-exact-commands.rego`](tools/examples/exec-exact-commands.rego))
2. **One program with plain arguments.** An anchored expression that admits
   only a program name and words made of characters the shell does not
   interpret, separated by single spaces:
   `^(git|ls)( [A-Za-z0-9_./:=@%+,-]+)*$`. With no space-in-word, `;`, `&`,
   `|`, `<`, `>`, `(`, `)`, `$`, `` ` ``, `\`, quote, `*`, `?`, `[`, `{`, `~`,
   `#`, `!` or newline, the shell runs exactly that program with exactly those
   words: the policy has recovered an argument list. The price: no argument
   with a space, no pipes.
   ([`exec-git-ls.rego`](tools/examples/exec-git-ls.rego),
   [`exec-no-metacharacters.rego`](tools/examples/exec-no-metacharacters.rego))
3. **A fixed shape with one free, quoted part.** Inside single quotes the
   shell interprets nothing, so `'[^']*'` is one argument whatever it
   contains. ([`exec-curl-hosts.rego`](tools/examples/exec-curl-hosts.rego))
4. **A fixed prefix**, such as the `cd` into a working directory, followed by
   a command of kind 2. ([`exec-workdir.rego`](tools/examples/exec-workdir.rego))
5. **Nothing.** ([`exec-deny.rego`](tools/examples/exec-deny.rego))

What does not work, and why every example is an anchored description of the
whole string:

- **Looking for bad substrings** (`not contains(cmd, "rm")`, a list of
  dangerous flags). The shell has many spellings for one thing: `r\m`,
  `$(printf rm)`, a variable, `eval`, a file it sources.
- **Checking how the string starts** (`startswith(cmd, "git ")`). What
  follows `;`, `&&`, `|`, a newline or `$( )` is another command.
- **Unanchored expressions.** `regex.match("git status", cmd)` matches
  `id; echo git status`. Begin with `^` and end with `$`. (In OPA, `$`
  matches only at the very end of the string, and none of the examples lets a
  newline through.)

And what no policy on this string can establish, however it is written:

- **What an allowed program goes on to run.** The policy decides the first
  program; that program decides the rest. `sh`, `env`, `xargs`, `find -exec`,
  `node`, `python3`, `make`, `npm` run arbitrary commands by design, and many
  others do with the right plain-word arguments
  (`git -c core.fsmonitor=/path/to/program status`, `tar --to-command=…`,
  `curl -K file`). A list of programs restricts an agent only as far as each
  program, with the arguments the policy lets through, cannot be made to run
  something else. Fix the arguments too (kinds 1 and 3) when it matters.
- **Which files a command touches.** The directory it starts in is not a
  wall: arguments can name any path the user can read or write.
- **How long it runs, exactly.** `timeout` is in seconds and the server has no
  maximum, so bound it in the policy. At the timeout mcp-exec kills the shell
  only; programs the shell started run on until they end.

What a command can reach, whatever the policy allowed it for:

- Everything the desktop's user can: the home directory, the session's disk at
  `/data/chrome` (the browser profile with its cookies and saved logins, the
  session's files, the logs of other commands), `/tmp`, and the X display (it
  can open windows, read the screen and the clipboard, and send input).
- The network the browser reaches: the internet, not the cluster, the node or
  the metadata address (the session NetworkPolicy).
- **The pod's loopback ports**: the browser container's own server
  (`127.0.0.1:8081`), Chromium's remote debugging port (`127.0.0.1:9222`) and
  mcp-exec itself (`127.0.0.1:8082`). A command that can make an HTTP request
  there drives the browser, or starts further commands, without passing
  mcp-js and so without any policy. Therefore: **a policy that restricts
  `browser_execute` must deny `exec`, or allow only commands that cannot make
  requests or run other programs; and a policy that restricts `exec` must
  deny `desktop_execute`**, which can type into a terminal on the desktop
  (`exec-deny.rego` denies both).

mcp-exec adds no sandbox. A command is confined by what already confines the
container: gVisor, an unprivileged user with every capability dropped and no
way to gain one, the pod's resource limits and its NetworkPolicy.

## The policies

Each is a complete tenant module in [`tools/examples/`](tools/examples/), with
a `.cases.json` of inputs and the decision it must give: hostile inputs
(`null` arguments, wrong types, the command under another field name) and,
for each, what a shell would do with a string that only starts well (`;`,
`&&`, a newline, `|`, `&`, `$( )`, backquotes, `>`, a variable, an assignment
before the program). `tools/gen-cases.py` writes the cases;
`tools/run-cases.py` runs them the way the cluster evaluates a policy (the
operator's tenant checks, `opa check` under `capabilities.json`, the package
rewritten to the tenant's, each case asked of the generated decision module)
and gives every accepted "one program with plain arguments" command line to
a real `sh`, which must split it into exactly the expected words. With OPA
1.9.0:

```
$ python3 docs/contracts/policy/tools/run-cases.py "$(command -v opa)" docs/contracts/policy
exec-curl-hosts: 57/57 cases pass (6 allow, 51 deny; 5 checked against sh)
exec-deny: 14/14 cases pass (1 allow, 13 deny; 0 checked against sh)
exec-exact-commands: 35/35 cases pass (5 allow, 30 deny; 0 checked against sh)
exec-git-ls: 46/46 cases pass (8 allow, 38 deny; 6 checked against sh)
exec-no-metacharacters: 38/38 cases pass (10 allow, 28 deny; 6 checked against sh)
exec-workdir: 43/43 cases pass (6 allow, 37 deny; 4 checked against sh)
233/233 cases pass
```

| Policy | Allows | Read this first |
|---|---|---|
| [`exec-exact-commands.rego`](tools/examples/exec-exact-commands.rego) | Three command lines, character for character, `timeout` 1 to 600; reading output. No browser or desktop tool. | The strongest, and the one to start from. |
| [`exec-git-ls.rego`](tools/examples/exec-git-ls.rego) | One `git` or `ls` command with plain-word arguments, `timeout` up to 600; reading output. Nothing else. | "Any arguments" includes the ones with which git runs another program; a case in the file shows one being allowed. It stops an agent from reaching for other tools, not a determined one. |
| [`exec-curl-hosts.rego`](tools/examples/exec-curl-hosts.rego) | `curl -q`, flags from a short list, one single-quoted `https://` URL whose host is `api.github.com` or `example.com`, `timeout` up to 120; reading output. | The whole line is one expression, which is why it holds. The host is followed by the end or a `/`, so userinfo (`https://example.com@evil.test/`), ports and look-alike hosts fail. `-L` is not on the list: a redirect goes wherever the server says. |
| [`exec-no-metacharacters.rego`](tools/examples/exec-no-metacharacters.rego) | Any one program with plain-word arguments; the browser and desktop tools; reading output. | A rule about form: no shell syntax. `sh /tmp/x.sh` is allowed by it (a case shows this); combine it with a list of programs to restrict what runs. |
| [`exec-workdir.rego`](tools/examples/exec-workdir.rego) | `cd /home/browser/work[/…] && <one program with plain arguments>`, no `..` anywhere, `timeout` up to 1800; reading output. | It decides where commands start. It does not confine them (a case shows a command reading an absolute path elsewhere being allowed). |
| [`exec-deny.rego`](tools/examples/exec-deny.rego) | `browser_execute` only. | Denies the exec server's three tools and `desktop_execute`. |

Patterns they share:

- `input.server == "exec"` and the tool by name in every rule: a tool called
  `exec` on another server is not this one.
- `is_string(input.arguments.cmd)` before any match; `is_number` and both
  bounds on `timeout`.
- Describe the whole string that is allowed, anchored at both ends; never
  look for what is dangerous.
- The home directory in the paths above is `/home/browser`, `$HOME` of the
  desktop's user in the current image; use the real one.

## What would make precise policies possible

Kinds 2 to 4 recover structure by forbidding most of the shell. A policy
could be both precise and unrestrictive if `exec` carried the structure
itself. Proposed for mcp-exec, not done here:

- `argv: [string]` as an alternative to `cmd`, executed directly with no
  shell, so that a policy reads the program and each argument as the values
  that run;
- `cwd` and `env` as fields, instead of `cd … &&` and `VAR=… ` inside the
  string;
- refusing unknown fields, so that a typo of a field name is an error and not
  silently ignored;
- at the timeout, killing the command's whole process group, so that
  `timeout` bounds what was started and not only the shell.
