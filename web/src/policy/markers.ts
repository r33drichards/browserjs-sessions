// Turns the server's diagnostics (a row and a column, no end) into editor
// markers. The end of the range is made up here: the rest of the token at
// that position, or the rest of the line. A diagnostic without a place (a
// warning about the policy as a whole) gets no marker: the problems pane
// shows it.
import type { Diagnostic } from "../policyApi"

export interface Marker {
  startLineNumber: number
  startColumn: number
  endLineNumber: number
  endColumn: number
  message: string
  code: string
  severity: "error" | "warning"
}

const TOKEN = /^("(?:[^"\\]|\\.)*"|[A-Za-z0-9_.$-]+|\S)/

export function toMarker(d: Diagnostic, source: string, severity: Marker["severity"]): Marker {
  const lines = source.split("\n")
  const row = Math.min(Math.max(d.row ?? 1, 1), Math.max(lines.length, 1))
  const line = lines[row - 1] ?? ""
  const col = Math.min(Math.max(d.col ?? 1, 1), line.length + 1)
  const token = d.col === undefined ? null : TOKEN.exec(line.slice(col - 1))
  const end = token ? col + token[0].length : line.length + 1
  return {
    startLineNumber: row,
    startColumn: col,
    endLineNumber: row,
    endColumn: Math.max(end, col + 1),
    message: d.message,
    code: d.code,
    severity,
  }
}

export function toMarkers(errors: Diagnostic[], warnings: Diagnostic[], source: string): Marker[] {
  const placed = (d: Diagnostic) => d.row !== undefined
  return [
    ...errors.filter(placed).map(d => toMarker(d, source, "error")),
    ...warnings.filter(placed).map(d => toMarker(d, source, "warning")),
  ]
}

// One line of the problems pane: "4:21  unknown operation".
export function problemLine(d: Pick<Diagnostic, "row" | "col" | "message">): string {
  const place = d.row === undefined ? "" : d.col === undefined ? `${d.row}  ` : `${d.row}:${d.col}  `
  return place + d.message
}
