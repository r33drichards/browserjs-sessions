# Write a policy

::: warning Coming, not yet enabled
Policies are built and switched off. This page describes how they work when
they are on. See [Live, coming and planned](/reference/status).
:::

A policy decides which of the agent's calls a session accepts. It does not
restrict you at the screen.

## Choose a starting point

When you create a session, choose a ready-made policy, copy one from another
session, or write your own. The ready-made ones:

| Policy | What the agent may do |
| --- | --- |
| Unrestricted | Everything: the browser, the desktop and the shell |
| Browser only | Every browser operation. No desktop control, no shell |
| No scripting | Everything in the browser except running script in a page or replacing its content. No desktop control, no shell |
| Observe only | Open https pages, wait, take screenshots. No desktop control, no shell |
| One site | Work on one site and its subdomains, with short typed text. No desktop control, no shell |
| Form filling | Fill in forms on the sites you name: printable text, a few keys, no script. No desktop control, no shell |
| Read-only shell | Everything in the browser, and a short list of read-only commands. No desktop control |

Each is a short Rego module with comments. Pick the closest and edit it.

## Write one

1. Open the session's **Policy** tab and choose **Edit**.
2. Say what is allowed. Everything else is refused, on every tool: a policy
   that only speaks of the browser refuses desktop control and the shell.

```txt
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		op.type in {"navigate", "screenshot", "url", "wait"}
	}
}
```

<<<<<<< HEAD
Programs are decided the same way. A call to run one arrives as the program
and its arguments, already separate, so a rule can name them. This policy
allows `git`, with two subcommands, and reading the output:

```txt
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	count(object.keys(input.arguments) - {"bin", "args", "timeout", "cwd"}) == 0
	input.arguments.bin == "git"
	input.arguments.args[0] in {"status", "log"}
	input.arguments.timeout <= 120
}

allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs", "kill"}
}
```

The third line of the first rule refuses a call with any other field, `env`
among them: environment variables such as `PATH` change what a program name
means. Compare `bin` as a whole (`"git"`, not "starts with git"), and deny a
shell (`bin` of `sh` or `bash`) unless every command is acceptable, since a
shell's command line cannot be judged by matching text. A policy like this
one, which does not mention the browser tools, refuses them.
=======
3. Try calls against it in the editor before saving.
4. Choose **Save policy**. It applies to the next call.
>>>>>>> a40aad5 (policies are Rego only, and cover every tool of a session)

The package name is fixed. It carries the product's earlier name, as a few
technical identifiers do.

If a policy restricts the browser, keep desktop control and the shell
refused: the mouse and keyboard, or a shell command, can drive the browser
around the rules. Saving a policy that leaves one open shows a warning.

## Manage it as code

Mark the policy as managed as code and keep it in Terraform or OpenTofu, or
send it through the API. The app then shows it read-only, with a link to
where it is managed. See [Use it from code](/guides/use-from-code).

## Check what it does not cover

A rule on `navigate` limits where the agent may send the browser. It does
not stop a page from linking or redirecting elsewhere. Read
[The containment model](/explanation/containment) before relying on a
policy.

The format is in [Policy format](/reference/policy).
