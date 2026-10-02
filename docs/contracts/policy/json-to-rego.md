# JSON policy to Rego

The translation is a pure function of the parsed JSON document. Its output
for each `examples/<name>.policy.json` is `examples/<name>.rego`, byte for
byte. Only the operator implements it (track A).

Input: a document valid against `json-policy.schema.json`. Anything else is
an error (`schema_error`, or `json_parse_error`), not a translation.

## Output, in order

Lines end with `\n`; indentation is one tab; the file ends with one `\n`.
A blank line separates the blocks below.

1. Header, exactly:

   ```
   # Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.
   package browserjs.policy

   import rego.v1
   ```

2. The entry rule, exactly:

   ```
   allow_tool_call if {
   	input.server == "browser"
   	input.tool == "browser_execute"
   	is_array(input.arguments.operations)
   	every op in input.arguments.operations {
   		operation_allowed(op)
   	}
   }
   ```

3. If `deny.operations` is not empty:
   `denied_operations := {…}`, the names as a set literal, sorted, each a
   JSON string, separated by `, `.

4. If `allow.operations` is not empty: `allowed_operations := {…}` in the
   same form, a blank line, then

   ```
   operation_allowed(op) if {
   	not op.type in denied_operations
   	op.type in allowed_operations
   }
   ```

   `"*"` anywhere in `allow.operations` stands for all eleven operation
   names of the schema; the set is then exactly those eleven. The
   `not op.type in denied_operations` line appears in this and every later
   block only when block 3 was written.

5. For each element of `allow.rules`, in the document's order, with its
   index `i`:

   ```
   # allow.rules[i]
   operation_allowed(op) if {
   	not op.type in denied_operations
   	op.type == "<operation>"
   	<checks>
   }
   ```

   `<checks>`: for each constrained parameter, in ascending order of name,
   with `P` standing for `op.params["<name>"]` (the name as a JSON string):

   | When the constraint has | Lines, in this order |
   |---|---|
   | `min` or `max` | `is_number(P)`, then `P >= <min>` if `min`, then `P <= <max>` if `max` |
   | any of `max_length`, `pattern`, `hosts`, `schemes` | `is_string(P)`, then `count(P) <= <max_length>` if `max_length`, then `regex.match(<pattern>, P)` if `pattern`, then `regex.match(<url regex>, lower(P))` if `hosts` or `schemes` |
   | `allowed` | `P in {…}`, the values each as JSON, sorted by their JSON text |

   Numbers and strings are written as JSON (`json.dumps` in Python, with
   non-ASCII characters left as they are). Patterns are therefore
   double-quoted Rego strings with JSON escaping.

Empty sets are never written: Rego treats `{}` as an object, and a rule
over a set that is provably empty is rejected by newer OPA versions.

## The URL regular expression

```
^(?:<schemes>)://<host>(?::[0-9]+)?(?:[/?#].*)?$
```

- `<schemes>`: the constraint's `schemes`, sorted, joined with `|`; `http|https`
  when only `hosts` is given.
- `<host>`: when `hosts` is given, `(?:<h1>|<h2>|…)` over the hosts sorted as
  strings, where an exact host is written with each `.` as `\.`, and
  `*.name` is written `(?:[a-z0-9-]+\.)+` followed by `name` with each `.`
  as `\.`. When `hosts` is absent, `[a-z0-9.-]+`.

It is matched against the lower-cased parameter. It deliberately matches
only plain URLs: userinfo, a backslash, whitespace, a percent-encoded or
non-ASCII host, a trailing dot, or a line break anywhere all fail, and so
does every scheme but the listed ones. `examples/one-site.cases.json` holds
those cases.

## Semantics the output must have

- Deny by default; `deny.operations` wins over everything.
- A call is allowed only when every operation in it is allowed. An empty
  list of operations is allowed (it does nothing).
- `arguments` that is null, or `operations` that is not an array, or an
  operation without a `type`, or a type the policy does not know: denied.
- A constrained parameter that is absent, or of the wrong type: the rule
  does not match.
- Parameters without a constraint, and the call's `tab` and `close`, are not
  examined.

## Warnings (not errors)

The translator returns these with the Rego; they do not stop a save.

| Code | When |
|---|---|
| `allow_empty` | nothing is allowed: no `allow.operations` and no `allow.rules` |
| `denied_and_allowed` | an operation is in both `deny.operations` and `allow` (deny wins) |
| `rule_shadowed` | an operation has a rule and is also in `allow.operations` (the rule adds nothing) |
| `unknown_parameter` | a constraint names a parameter the operation does not take (the table below) |

Parameters of each operation, from `images/browser/browser/server.js`:

| Operation | Parameters |
|---|---|
| `setViewport` | `width`, `height` |
| `navigate` | `url`, `waitUntil` |
| `setContent` | `html` |
| `wait` | `ms`, `selector` |
| `screenshot` | `fullPage` |
| `evaluate` | `script` |
| `click` | `selector` |
| `type` | `selector`, `text`, `delay` |
| `press` | `key` |
| `select` | `selector`, `values` |
| `url` | none |
