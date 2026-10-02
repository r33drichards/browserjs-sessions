// Monaco from the npm package, never a CDN: the editor core and its worker,
// built into this app's own assets. No bundled language: Rego's grammar is
// rego.ts, and the problems come from the server.
// Imported only by MonacoEditor.tsx, which is a lazy chunk.
import * as monaco from "monaco-editor/esm/vs/editor/edcore.main"
import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker"
import { REGO_LANGUAGE_ID, regoConfiguration, regoMonarch } from "./rego"

export const THEME = "wireframe"

let ready = false

export function setupMonaco(): typeof monaco {
  if (ready) return monaco
  ready = true

  self.MonacoEnvironment = { getWorker: () => new EditorWorker() }

  monaco.languages.register({ id: REGO_LANGUAGE_ID, extensions: [".rego"] })
  monaco.languages.setLanguageConfiguration(REGO_LANGUAGE_ID, regoConfiguration)
  monaco.languages.setMonarchTokensProvider(REGO_LANGUAGE_ID, regoMonarch as monaco.languages.IMonarchLanguage)

  // The wireframe look: ink on paper, weight and slant instead of colour.
  monaco.editor.defineTheme(THEME, {
    base: "vs",
    inherit: false,
    rules: [
      { token: "", foreground: "111111" },
      { token: "comment", foreground: "6b6b6b", fontStyle: "italic" },
      { token: "keyword", foreground: "111111", fontStyle: "bold" },
      { token: "type.identifier", foreground: "111111", fontStyle: "underline" },
      { token: "string", foreground: "444444" },
      { token: "number", foreground: "111111" },
      { token: "delimiter", foreground: "111111" },
      { token: "delimiter.bracket", foreground: "111111" },
      { token: "constant", foreground: "111111", fontStyle: "italic" },
    ],
    colors: {
      "editor.background": "#ffffff",
      "editor.foreground": "#111111",
      "editorLineNumber.foreground": "#6b6b6b",
      "editorLineNumber.activeForeground": "#111111",
      "editorCursor.foreground": "#111111",
      "editor.selectionBackground": "#dddddd",
      "editor.lineHighlightBackground": "#f6f6f6",
      "editorError.foreground": "#111111",
      "editorWarning.foreground": "#6b6b6b",
      "editorIndentGuide.background1": "#e4e4e4",
      // Brackets are not coloured by depth.
      ...Object.fromEntries([1, 2, 3, 4, 5, 6].map(n => [`editorBracketHighlight.foreground${n}`, "#111111"])),
      "editorBracketHighlight.unexpectedBracket.foreground": "#111111",
    },
  })
  return monaco
}
