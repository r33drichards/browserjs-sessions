# The `exec` server (mcp-exec): what a policy sees

For someone writing a session policy in Rego (`rego-contract.md`: package
`browserjs.policy`, entry rule `allow_tool_call`) that allows, denies or
constrains the programs an agent runs.

Commands are run by [mcp-exec](https://github.com/r33drichards/mcp-exec),
which mcp-js knows as the upstream server `"exec"`
(`computer-use-mcp/code-mode/mcp-servers.json`). It runs inside the browser container
(`computer-use-mcp/browser/exec-server.sh`), so a command runs on the desktop:
as its user, in its home directory, with its `PATH` and `DISPLAY`.

Machine-readable: [`tools/exec.schema.json`](tools/exec.schema.json),
[`tools/stream_logs.schema.json`](tools/stream_logs.schema.json),
[`tools/search_logs.schema.json`](tools/search_logs.schema.json),
[`tools/kill.schema.json`](tools/kill.schema.json), and a sample
input for each next to them. Policies and their cases:
[`tools/examples/`](tools/examples/).

## The input

Agent code in `run_js` calls

```js
await mcp.callTool("exec", "exec", { bin: "git", args: ["status", "--short"], cwd: "/data/chrome/home/work/app", timeout: 60 });
```

and mcp-js v0.21.0-rc.4 asks the policy before it forwards the call, with
(`server/src/engine/mcp_client.rs`, `McpToolPolicyInput`):

```json
{
  "operation": "mcp_call_tool",
  "server": "exec",
  "tool": "exec",
  "arguments": {
    "bin": "git",
    "args": ["status", "--short"],
    "cwd": "/data/chrome/home/work/app",
    "timeout": 60
  }
}
```

`arguments` is the object exactly as the agent's code passed it: only the
fields it gave, no defaults filled in, or `null` when it passed none.

| Tool | `arguments` | What it does |
|---|---|---|
| `exec` | `bin`: string; `args`: array of strings, optional; `timeout`: integer, **seconds**; `cwd`: string, optional; `env`: object of strings, optional | Runs the program `bin` with the arguments `args`, directly, and answers at once with `{id, status: "started"}`. The command goes on running. |
| `stream_logs` | `id`: string (the UUID); `offset`: integer | The command's output from that byte on, where to continue, and its status (`running`, `completed:<exit code>`, `timeout`, `cancelled`, `failed:…`). |
| `search_logs` | `id`: string; `pattern`: string | The lines of the output matching a regular expression. |
| `kill` | `id`: string | Stops a running command, with everything it started. |

These are the fields of `ExecRequest`, `StreamLogsRequest`,
`SearchLogsRequest` and `KillRequest` in mcp-exec's `src/service.rs` at the
commit the image pins (`computer-use-mcp/flake.lock`); the image build compares
them with what the packaged server lists
(`computer-use-mcp/test/exec-smoke.mjs`).

What mcp-exec does with an `exec` call:

- **No shell.** `bin` is executed with `args` as its argument list, each
  argument passed as the string it is. Nothing is split, expanded or
  interpreted: `args: ["; rm -rf ~", "$(id)", "*"]` are three odd strings the
  program receives. There is no command-line form (`cmd` existed in mcp-exec
  0.1 and is refused now).
- **`bin`** without a `/` is a name looked up on `PATH`; with one it is a
  path, relative to `cwd` unless it is absolute.
- **`args`** absent means no arguments, the same as `[]`.
- **`cwd`** must be an absolute path to an existing directory, and is used as
  written: `..` segments and symbolic links are followed, not refused.
  Absent: the desktop user's home directory.
- **`env`** is added to the desktop's environment. Any name is accepted,
  `PATH` and `LD_PRELOAD` included, and a `PATH` given here is the one `bin`
  is looked up on.
- **Unknown fields are refused**, and so are an empty `bin`, a `cwd` that is
  not such a directory, and an `env` name that is empty or contains `=`.

**The policy is asked before the server checks anything.** Every field can be
missing or of any type, and other fields can be present. The server will
refuse such a call afterwards, but a policy must not allow it on the strength
of a field it misread: test types (`is_string`, `is_array`, every argument a
string), and name the fields you accept
(`object.keys(input.arguments) - known`) rather than the ones you reject.

`stream_logs`, `search_logs` and `kill` start nothing. A policy that allows
some commands can allow them without looking at their arguments; one that
allows no commands can deny them with everything else. There is no tool that
lists commands.

## What a policy can decide

The call is the command, already taken apart: the program is one value, each
argument is one value, the directory and the variables are fields. A policy
reads the values that will run, so it can decide on:

- **The program**: `input.arguments.bin == "git"`, or `in` a set. As a whole
  string: `"/usr/bin/git"`, `"./git"` and `"git "` are other strings.
- **A subcommand or any position**: `args[0] in {"status", "log"}`.
- **Every argument**: `every arg in args { … }`, to allow only known flags, or
  to refuse some.
- **A value inside an argument**: a URL's host, a path's prefix, with an
  anchored expression on that one argument.
- **The whole command**: `{bin, args, cwd}` equal to an entry of a list.
- **Where it starts**: `cwd` given and under a directory.
- **The environment**: `env` absent, or its names from a list.
- **How long**: `timeout` within a bound (the server has none).

Three things a policy has to do for that to hold:

- **Deny `env`, or list the names it may set.** `PATH` changes which file a
  name in `bin` means; `LD_PRELOAD`, `GIT_SSH_COMMAND`, `NODE_OPTIONS` and
  many others make a program load or run something else.
- **Check `cwd` itself** when it matters: refuse `..`, `.` and empty segments,
  since the server follows them. A symbolic link inside an allowed directory
  that leads out of it cannot be seen from the policy.
- **Treat `args` absent as `[]`**, and require an array of strings; the
  examples share a helper, `call_args`, that does both.

And what no policy on these arguments can establish:

- **What an allowed program goes on to run.** The policy decides the first
  program; that program decides the rest. Some exist to run others (`sh`,
  `bash`, `env`, `xargs`, `nohup`, `timeout`, interpreters such as `python3`
  and `node`, `make`, `npm`), and many more do it with the right arguments or
  files: `find -exec`, `git -c alias.x='!cmd' x`, `git -c core.fsmonitor=cmd
  status`, a repository's own git configuration, `tar --to-command`,
  `curl -K file`, a `Makefile`, a `package.json` script. A list of programs
  restricts an agent only as far as each program, with the arguments the
  policy lets through and the files it will read, cannot be made to run
  something else. Fix the arguments too when it matters.
- **A shell's command line.** `bin: "sh", args: ["-c", "…"]` is visible as
  such, and a policy can deny it. If it allows it, the string after `-c` is a
  program in a language with many spellings for one thing (`c\url`,
  `$(printf cu)rl`, `eval`): matching text in it establishes nothing.
- **Which files a command touches.** `cwd` is where it starts. Arguments can
  name any path the user can read or write.

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
a `.cases.json` of inputs and the decision it must give, hostile inputs
included: `null` arguments, `bin` or `args` of the wrong type, an argument
that is not a string, the old `cmd` form alone and beside `bin`, unknown
fields, `PATH` and `LD_PRELOAD` in `env`. `tools/gen-cases.py` writes the
cases; `tools/run-cases.py` runs them the way the cluster evaluates a policy:
the operator's tenant checks, `opa check` under `capabilities.json`, the
package rewritten to the tenant's, each case asked of the generated decision
module. With OPA 1.9.0:

```
$ python3 docs/contracts/policy/tools/run-cases.py "$(command -v opa)" docs/contracts/policy
exec-curl-hosts: 67/67 cases pass (9 allow, 58 deny)
exec-deny: 25/25 cases pass (1 allow, 24 deny)
exec-exact-commands: 41/41 cases pass (6 allow, 35 deny)
exec-git-subcommands: 55/55 cases pass (8 allow, 47 deny)
exec-no-shell: 40/40 cases pass (12 allow, 28 deny)
exec-workdir: 49/49 cases pass (10 allow, 39 deny)
277/277 cases pass
```

| Policy | Allows | Read this first |
|---|---|---|
| [`exec-exact-commands.rego`](tools/examples/exec-exact-commands.rego) | Three commands, each a `{bin, args, cwd}` matched exactly, `timeout` 1 to 600; reading output and `kill`. No `env`, no browser or desktop tool. | The strongest, and the one to start from. |
| [`exec-git-subcommands.rego`](tools/examples/exec-git-subcommands.rego) | `git` with `args[0]` one of `status`, `log`, `diff`, `show`, `rev-parse`, `ls-files`, and none of four long flags (matched by prefix, since git accepts abbreviations); `timeout` up to 120. Nothing else. | Requiring the subcommand first keeps git's own `-c` and `-C` out. The denied flags are a list of what its author knew, and git obeys the configuration of the repository it runs in: safe for repositories the agent cannot write to. |
| [`exec-curl-hosts.rego`](tools/examples/exec-curl-hosts.rego) | `curl` with `-q` first, flags from a short list, and `https://` URLs whose host is `api.github.com` or `example.com`; `timeout` up to 120. | Every argument is accounted for, which is why it holds: an unknown flag denies. The host is followed by the end or a `/`, so userinfo (`https://example.com@evil.test/`), ports and look-alike hosts fail. `-L` is not on the list: a redirect goes wherever the server says. |
| [`exec-no-shell.rego`](tools/examples/exec-no-shell.rego) | Any program whose name (the last part of `bin`) is not on a list of shells, interpreters and other launchers; no `env`; the browser and desktop tools. | A deny-list, so only as good as the list. Two cases show what it still allows: `find … -exec sh -c …`, and a copy of the shell under another name. It keeps commands readable; it does not restrict what runs. |
| [`exec-workdir.rego`](tools/examples/exec-workdir.rego) | Any program, with `cwd` given and equal to or under `/data/chrome/home/work` (no `..`, `.` or empty segment), `env` absent or its names from a list of five, `timeout` up to 1800. | It decides where commands start. It does not confine them (a case shows a command reading an absolute path elsewhere being allowed), and it cannot see a symbolic link that leads out. |
| [`exec-deny.rego`](tools/examples/exec-deny.rego) | `browser_execute` only. | Denies the exec server's tools and `desktop_execute`. |

Patterns they share:

- `input.server == "exec"` and the tool by name in every rule: a tool called
  `exec` on another server is not this one.
- `only_fields({...})`: the call has no field the policy has not looked at.
  A call carrying `env`, or a field added to the tool later, is denied until
  the policy says otherwise.
- `call_args`: the arguments as an array of strings, `[]` when `args` is
  absent, undefined (so the call is denied) when it is anything else.
- `timeout_within(n)`: a number, at least 1, at most `n`.
- Describe what is allowed and deny the rest. The one list of denied things
  that is not backed by a list of allowed ones, `exec-no-shell.rego`, says so.
- The home directory in the paths above is `/data/chrome/home`, `$HOME` of the
  desktop's user in the current image; use the real one.
