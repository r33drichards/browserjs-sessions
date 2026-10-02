# `shell_execute` and `shell_process`: what a policy sees

For someone writing a session policy in Rego (`rego-contract.md`: package
`browserjs.policy`, entry rule `allow_tool_call`) that allows, denies or
constrains commands run with the browser server's `shell_execute` tool.

Machine-readable: [`tools/shell_execute.schema.json`](tools/shell_execute.schema.json),
[`tools/shell_process.schema.json`](tools/shell_process.schema.json).
Policies and their cases: [`tools/examples/`](tools/examples/).

## The input

Agent code in `run_js` calls

```js
await mcp.callTool("browser", "shell_execute", { argv: ["git", "status"], cwd: "/home/browser/work" });
```

and mcp-js v0.21.0-rc.3 asks the policy before it forwards the call, with
(`server/src/engine/mcp_client.rs`, `McpToolPolicyInput`):

```json
{
  "operation": "mcp_call_tool",
  "server": "browser",
  "tool": "shell_execute",
  "arguments": {
    "argv": ["git", "status"],
    "cwd": "/home/browser/work"
  }
}
```

`arguments` is the object exactly as the agent's code passed it: only the
fields it gave, no defaults filled in, or `null` when it passed none. The
samples are [`tools/shell_execute.input-sample.json`](tools/shell_execute.input-sample.json)
and [`tools/shell_process.input-sample.json`](tools/shell_process.input-sample.json).

**The policy is asked before the tool checks anything.** Every field can be
missing or of any type, and fields the tool does not have can be present. The
tool will refuse such a call afterwards, but a policy must not allow it on
the strength of a field it misread: test types (`is_array`, `is_string`), and
prefer naming the fields you accept (`object.keys(input.arguments) -
understood_fields`) to naming the ones you reject.

### `arguments` of `shell_execute`

Exactly one of `argv` and `script`; no other fields than these.

| Field | Type | What the tool does with it |
|---|---|---|
| `argv` | array of strings, 1 to 1024 | Runs `argv[0]` with the rest as its arguments, directly: no shell, no word splitting, no globbing, no expansion. `argv[0]` without a `/` is looked up on the server's `PATH`; with one, it is that path (relative to `cwd`). |
| `script` | string, 1 to 131072 bytes | Runs `bash -lc <script>`. |
| `cwd` | string | The working directory. Refused unless it is absolute, written normalised (no `.`, `..`, `//`, trailing `/`), an existing directory with no symbolic link anywhere in the path, and equal to or inside the home directory, `/data` or `/tmp`. So the string a policy reads is the directory the command starts in. Absent: the home directory. |
| `env` | object, name to string, at most 64 | Added to the environment the desktop runs with. Names must match `^[A-Za-z_][A-Za-z0-9_]*$`. Refused: `PATH`, `BASH_ENV`, `ENV`, `SHELLOPTS`, `BASHOPTS`, `PS4`, `NODE_OPTIONS`, any `LD_*`, any `BASH_FUNC_*`. |
| `stdin` | string, at most 1 MiB | Written to the command's input. |
| `timeout_ms` | integer | Foreground: 1 to 600000, default 30000. Background: 1 to 86400000, default none. |
| `max_output_bytes` | integer, 1 to 4194304 | Bytes kept per stream; default 1048576. |
| `background` | boolean | `true`: the call returns a handle at once and the command keeps running. |

The home directory is `$HOME` of the desktop's user: `/home/browser` in the
current image.

### `arguments` of `shell_process`

`{action: "list"}`, `{action: "poll", id, wait_ms?}`,
`{action: "kill", id, signal?}`. It reads the output of, and ends, commands
that an allowed `shell_execute` call started with `background: true`. It
cannot start a command or change one, so a policy that restricts which
commands start can allow it without looking at its arguments, and a policy
that allows no commands can deny it with everything else.

## What the tool guarantees, and what it does not

What a policy can rely on, once it has allowed a call the tool accepts:

- With `argv`, the program that starts is `argv[0]` and its arguments are the
  rest, byte for byte. Nothing in the call can make a name mean another
  program: `env` cannot set `PATH` or the loader's and bash's variables, and
  there is no other field that names a command.
- The command starts in `cwd` as written, or in the home directory.
- It runs as the desktop's user (uid 1000 in a session), with no terminal.

What no policy on these arguments can establish:

- **What `script` does.** It is a program in a string. A policy can match
  substrings, and bash has many ways to write the same thing (`c\url`,
  `$(printf cu)rl`, a variable, `eval`, a file it sources). Deny the `script`
  form unless every script is acceptable.
- **What an allowed program goes on to run.** The policy decides the first
  program; that program decides the rest. `bash`, `sh`, `env`, `xargs`,
  `find -exec`, `node`, `python3`, `make`, `npm` and editors run arbitrary
  commands by design, and many others do with the right arguments
  (`git -c alias.x='!sh -c ...' x`, `git -c core.sshCommand=...`,
  `tar --to-command`, `curl -K file`). An allow-list of programs restricts an
  agent only as far as each listed program, with the arguments the policy
  lets through, cannot be made to run something else. Constrain the arguments
  (`tools/examples/shell-curl-hosts.rego`) or list only programs that have no
  such feature.
- **Which files a command touches.** `cwd` is where it starts. Arguments can
  name any path the user can read or write, and a relative `argv[0]` or a
  program found through a directory on `PATH` that the user can write to is
  whatever file is there.
- **What the name on `PATH` is.** `PATH` is the server's, fixed when the image
  was built, and the image's programs are read-only; a policy that allows
  writing files (a download, `git clone`, `curl -o`) and then running them by
  path has allowed any program.

What a command can reach, whatever the policy allowed it for:

- Everything the desktop's user can: the home directory, the session's disk at
  `/data/chrome` (the browser profile with its cookies and saved logins, the
  session's files), `/tmp`, and the X display (it can open windows, read the
  screen and the clipboard, and send input).
- The network the browser reaches: the internet, not the cluster, the node or
  the metadata address (the session NetworkPolicy), and the policy server's
  decision endpoint, like every process in the pod.
- **The other containers' ports on loopback**, among them this server
  (`127.0.0.1:8081`) and Chromium's remote debugging port (`127.0.0.1:9222`).
  A command that can make an HTTP request there calls `browser_execute`, or
  drives Chromium, without passing mcp-js and so without any policy.
  Therefore: **a policy that restricts `browser_execute` or `desktop_execute`
  must deny `shell_execute`, or allow only commands that cannot make requests
  or run other programs.** The same holds the other way round for
  `desktop_execute`: it can type into a terminal on the desktop, so a policy
  that restricts commands must deny it (`tools/examples/shell-deny.rego`).

The tool adds no sandbox of its own. A command is confined by what already
confines the container: gVisor, an unprivileged user with every capability
dropped and no way to gain one, the pod's resource limits and its
NetworkPolicy. The `cwd` roots and the refused variables exist so that the
arguments mean what they say to a policy, not to contain the command.

## Five policies

Each is a complete tenant module in [`tools/examples/`](tools/examples/), with
a `.cases.json` of inputs and the decision it must give (hostile inputs
included: `null` arguments, wrong types, both forms at once).
`tools/gen-cases.py` writes the cases; `tools/run-cases.py` runs them the way
the cluster evaluates a policy: the operator's tenant checks, `opa check`
under `capabilities.json`, the package rewritten to the tenant's, each case
asked of the generated decision module. With OPA 1.9.0:

```
$ python3 docs/contracts/policy/tools/run-cases.py "$(command -v opa)" docs/contracts/policy
shell-curl-hosts: 49/49 cases pass (6 allow, 43 deny)
shell-deny: 14/14 cases pass (1 allow, 13 deny)
shell-git-ls: 34/34 cases pass (7 allow, 27 deny)
shell-no-script: 17/17 cases pass (6 allow, 11 deny)
shell-workdir: 25/25 cases pass (6 allow, 19 deny)
139/139 cases pass
```

| Policy | Allows | Read this first |
|---|---|---|
| [`shell-git-ls.rego`](tools/examples/shell-git-ls.rego) | `argv` whose program is `git` or `ls`, any arguments; `shell_process`. Nothing else: no `script`, no `env`, no browser or desktop tool. | "Any arguments" includes the ones with which git runs other programs; a case in the file shows one being allowed. It stops an agent from reaching for other tools, not a determined one. |
| [`shell-curl-hosts.rego`](tools/examples/shell-curl-hosts.rego) | `curl -q`, flags from a short list, and `https://` URLs whose host is `api.github.com` or `example.com`; `shell_process`. | Every argument is accounted for, which is why it holds: an unknown flag denies. The URL is matched with one anchored expression that takes the host up to the first `/`, so userinfo (`https://example.com@evil.test/`), ports and look-alike hosts fail. `-L` is not on the list: a redirect goes wherever the server says. |
| [`shell-no-script.rego`](tools/examples/shell-no-script.rego) | The `argv` form, and the other tools. Denies any call that has a `script` field. | A rule about form. `argv: ["bash", "-c", ...]` is allowed by it (a case shows this); combine it with a list of programs to restrict what runs. |
| [`shell-workdir.rego`](tools/examples/shell-workdir.rego) | Commands, in either form, whose `cwd` is given and is `/home/browser/work` or below; `shell_process`. | It decides where commands start. It does not confine them (two cases show a command reading elsewhere being allowed). |
| [`shell-deny.rego`](tools/examples/shell-deny.rego) | `browser_execute` only. | Denies `shell_execute`, `shell_process` and `desktop_execute`, which could type into a terminal. |

Patterns the five share:

- `input.server == "browser"` and the tool by name in every rule: another
  server's tool of the same name is not this one.
- `count(object.keys(input.arguments) - understood_fields) == 0`: deny what
  the policy does not understand, so that a field added to the tool later is
  denied until the policy is revisited, and `argv` together with `script`
  never slips through on the strength of the `argv`.
- Compare `argv[0]` to exact names. `"/usr/bin/git"`, `"./git"` and `"git "`
  are different strings and, unless listed, denied.
- When constraining arguments, describe what is allowed and deny the rest;
  never look for the dangerous flags.
