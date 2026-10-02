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
| Unrestricted | Every browser operation |
| No scripting | Everything except running script in a page or replacing its content |
| Observe only | Open https pages, wait, take screenshots |
| One site | Work on one site and its subdomains, with short typed text |
| Form filling | Fill in forms on the sites you name: printable text, a few keys, no script |

## Write one in JSON

1. Open the session's **Policy** tab and choose **Edit**.
2. Say what is allowed. Everything else is refused.

```json
{
  "version": 1,
  "description": "Only example.com, and only short text.",
  "allow": {
    "operations": ["click", "press", "select", "wait", "screenshot", "url"],
    "rules": [
      { "operation": "navigate",
        "constraints": { "url": { "schemes": ["https"], "hosts": ["example.com", "*.example.com"] } } },
      { "operation": "type",
        "constraints": { "text": { "max_length": 500 } } }
    ]
  }
}
```

3. Try calls against it in the editor before saving.
4. Choose **Save policy**. It applies to the next call.

## Write one in Rego

Use Rego when JSON cannot say it, for example to decide calls to tools other
than the browser.

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

The package name is fixed. It carries the product's earlier name, as a few
technical identifiers do.

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
