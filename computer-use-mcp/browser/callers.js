// Who may call the MCP endpoint.
//
// The only intended caller is mcp-js, in the same pod, over loopback. But the
// port is also reachable by the Chromium this server drives, so a web page
// open in the session could send requests here and act on the browser without
// passing through mcp-js and its policies. A browser cannot hide what it is:
// requests made by a page carry Origin and Sec-Fetch-* headers that page
// script cannot remove, and a request with a JSON content type is preflighted.
// mcp-js sends none of those headers.
export function mcpCallerRefusal(req) {
  const h = req.headers;
  const host = String(h.host || '').replace(/:\d+$/, '');
  if (host !== 'localhost' && host !== '127.0.0.1' && host !== '[::1]') return 'host';
  if (h.origin !== undefined) return 'origin';
  for (const name of Object.keys(h)) if (name.startsWith('sec-fetch-')) return name;
  const type = String(h['content-type'] || '').split(';')[0].trim().toLowerCase();
  if (type !== 'application/json') return 'content-type';
  return null;
}
