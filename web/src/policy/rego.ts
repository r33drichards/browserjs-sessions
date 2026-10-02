// A Monarch grammar for Rego (v1 syntax). Monaco has no Rego language of its
// own. Plain data, so it can be checked without loading the editor.
export const REGO_LANGUAGE_ID = "rego"

export const regoKeywords = [
  "package", "import", "as", "default", "if", "contains", "else", "every", "in", "some", "not", "with",
]

export const regoConfiguration = {
  comments: { lineComment: "#" },
  brackets: [
    ["{", "}"],
    ["[", "]"],
    ["(", ")"],
  ] as [string, string][],
  autoClosingPairs: [
    { open: "{", close: "}" },
    { open: "[", close: "]" },
    { open: "(", close: ")" },
    { open: '"', close: '"', notIn: ["string"] },
    { open: "`", close: "`", notIn: ["string"] },
  ],
}

export const regoMonarch = {
  defaultToken: "",
  keywords: regoKeywords,
  constants: ["true", "false", "null"],
  operators: [":=", "==", "!=", "<=", ">=", "<", ">", "=", "+", "-", "*", "/", "%", "&", "|"],
  symbols: /[=><!:+\-*/%&|]+/,
  escapes: /\\(?:["\\/bfnrt]|u[0-9A-Fa-f]{4})/,
  tokenizer: {
    root: [
      [/#.*$/, "comment"],
      // A name followed by "(" is a call or a function head: a.b.c(
      [/[a-zA-Z_][\w.]*(?=\()/, { cases: { "@keywords": "keyword", "@default": "type.identifier" } }],
      [/[a-zA-Z_]\w*/, { cases: { "@keywords": "keyword", "@constants": "constant", "@default": "identifier" } }],
      [/-?\d+(\.\d+)?([eE][+-]?\d+)?/, "number"],
      [/"/, "string", "@string"],
      [/`/, "string", "@rawstring"],
      [/[{}()[\]]/, "@brackets"],
      [/@symbols/, { cases: { "@operators": "operator", "@default": "" } }],
      [/[,;.]/, "delimiter"],
    ],
    string: [
      [/[^\\"]+/, "string"],
      [/@escapes/, "string.escape"],
      [/\\./, "string.escape.invalid"],
      [/"/, "string", "@pop"],
    ],
    rawstring: [
      [/[^`]+/, "string"],
      [/`/, "string", "@pop"],
    ],
  },
}

// What the editor starts from when a policy is written by hand.
export const REGO_TEMPLATE = `# What an agent may do in this session. The policy is asked about every tool
# call (input.server, input.tool, input.arguments) and a call is refused
# unless a rule allows it: desktop_execute and the exec server's tools (exec,
# stream_logs, search_logs) are refused until a rule here allows them.
#
# Restricting the browser while allowing desktop control or the shell can be
# walked around: desktop_execute can type into the address bar or DevTools,
# and a shell command can reach the browser's own control ports.
package browserjs.policy

import rego.v1

# Allow a browser_execute call when every operation in it is allowed.
allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	is_array(input.arguments.operations)
	every op in input.arguments.operations {
		op.type in {"navigate", "click", "type", "press", "wait", "screenshot", "url"}
	}
}
`
