// The app's API clients take `fetch` when their modules load. Tests that
// render pages point this at a mock backend (see harness.tsx) afterwards.
declare global {
  // eslint-disable-next-line no-var
  var __testFetch: typeof fetch | undefined
}

const real = globalThis.fetch
globalThis.fetch = (input, init) => (globalThis.__testFetch ?? real)(input, init)

export {}

// Tell React the tests wrap their updates (Testing Library does the wrapping).
;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
