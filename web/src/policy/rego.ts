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

// What the editor starts from when a policy is written in Rego by hand.
export const REGO_TEMPLATE = `package browserjs.policy

import rego.v1

# Allow a browser_execute call when every operation in it is allowed.
allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		op.type in {"navigate", "click", "type", "press", "wait", "screenshot", "url"}
	}
}
`
