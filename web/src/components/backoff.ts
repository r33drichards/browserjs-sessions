const BASE_MS = 2000
const CAP_MS = 30000

// Delay before reconnect number `attempt` (0-based): 2s, 4s, 8s … capped at 30s.
export function reconnectDelay(attempt: number): number {
  return Math.min(CAP_MS, BASE_MS * 2 ** Math.max(0, attempt))
}
