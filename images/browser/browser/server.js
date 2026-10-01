#!/usr/bin/env node

/**
 * Browser automation MCP server (Streamable HTTP).
 *
 * Unlike a launch-per-call server, this attaches over CDP to ONE long-lived,
 * headed Chromium (started by entrypoint.sh on Xvfb, profile on a volume), so
 * logins persist and a human can watch/drive the same browser over noVNC.
 * Tool calls run in named tabs that stay open and are reused across calls.
 *
 * Reachable only on the Railway private network; mcp-js (JWT auth + OPA
 * policy) is the public entry point.
 */

import fs from 'node:fs';
import http from 'node:http';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';
import puppeteer from 'puppeteer-core';

const CDP_URL = process.env.CDP_URL || 'http://127.0.0.1:9222';
const PORT = Number(process.env.BROWSER_MCP_PORT || 8081);

const DEFAULT_WIDTH = 1280;
const DEFAULT_HEIGHT = 800;
const MAX_WIDTH = 3840;
const MAX_HEIGHT = 2160;
const MAX_WAIT_MS = 30000;
const NAV_TIMEOUT_MS = 45000;

let browserPromise = null;

async function getBrowser() {
  if (browserPromise) {
    const browser = await browserPromise.catch(() => null);
    if (browser?.connected) return browser;
  }
  browserPromise = puppeteer.connect({ browserURL: CDP_URL, defaultViewport: null });
  const browser = await browserPromise;
  browser.once('disconnected', () => {
    browserPromise = null;
  });
  return browser;
}

async function runOperation(page, operation, images) {
  const { type, params = {} } = operation;

  switch (type) {
    case 'setViewport': {
      const width = Math.min(Math.max(params.width || DEFAULT_WIDTH, 320), MAX_WIDTH);
      const height = Math.min(Math.max(params.height || DEFAULT_HEIGHT, 200), MAX_HEIGHT);
      await page.setViewport({ width, height });
      return { width, height };
    }

    case 'navigate': {
      // Social sites never reach networkidle0 (long-polling), so default to
      // domcontentloaded and follow with an explicit `wait` on a selector.
      await page.goto(params.url, {
        waitUntil: params.waitUntil || 'domcontentloaded',
        timeout: NAV_TIMEOUT_MS,
      });
      return { url: page.url() };
    }

    case 'setContent': {
      await page.setContent(params.html, { waitUntil: 'networkidle0' });
      return { loaded: true };
    }

    case 'wait': {
      const ms = Math.min(params.ms || 0, MAX_WAIT_MS);
      if (params.selector) {
        await page.waitForSelector(params.selector, { timeout: ms || 10000, visible: true });
        return { waited_for: params.selector };
      }
      await new Promise((resolve) => setTimeout(resolve, ms));
      return { waited_ms: ms };
    }

    case 'screenshot': {
      const data = await page.screenshot({ type: 'png', fullPage: params.fullPage || false, encoding: 'base64' });
      images.push(data);
      return { image_index: images.length - 1 };
    }

    case 'evaluate': {
      const result = await page.evaluate(params.script);
      return { result };
    }

    case 'click': {
      await page.waitForSelector(params.selector, { timeout: 10000, visible: true });
      await page.click(params.selector);
      return { clicked: params.selector };
    }

    case 'type': {
      await page.waitForSelector(params.selector, { timeout: 10000, visible: true });
      await page.click(params.selector);
      await page.type(params.selector, params.text, { delay: params.delay ?? 15 });
      return { typed: params.text.length + ' chars', selector: params.selector };
    }

    case 'press': {
      await page.keyboard.press(params.key);
      return { pressed: params.key };
    }

    case 'select': {
      const values = await page.select(params.selector, ...params.values);
      return { selected: values };
    }

    case 'url': {
      return { url: page.url(), title: await page.title() };
    }

    default:
      throw new Error(`Unknown operation: ${type}`);
  }
}

// Named tabs live across calls so a caller can keep driving the same page
// (and the VNC viewer doesn't see a tab flash open and shut per call).
const tabs = new Map(); // name -> Page
const tabQueues = new Map(); // name -> tail of that tab's pipeline queue

// This process forgets `tabs` when it restarts, while Chromium restores its
// pages (--restore-last-session). Remember name -> URL on disk so a name is
// rebound to its restored page instead of opening a blank tab beside it.
const TAB_STATE = process.env.TAB_STATE_FILE || '';
let savedTabs = {};
if (TAB_STATE) {
  try {
    savedTabs = JSON.parse(fs.readFileSync(TAB_STATE, 'utf8'));
  } catch {}
}

function saveTabs() {
  if (!TAB_STATE) return;
  // Names not rebound yet since the restart keep their saved URL.
  const state = { ...savedTabs };
  for (const [name, page] of tabs) if (!page.isClosed()) state[name] = page.url();
  savedTabs = state;
  try {
    fs.writeFileSync(TAB_STATE, JSON.stringify(state));
  } catch {}
}

async function getTab(name) {
  const existing = tabs.get(name);
  // Closed over VNC, or Chromium restarted underneath us: start over.
  if (existing && !existing.isClosed() && existing.browser().connected) return existing;

  const browser = await getBrowser();
  // Prefer the restored page this name last had, then adopt a spare blank
  // tab (Chromium's startup tab) before opening another.
  const owned = new Set(tabs.values());
  const pages = await browser.pages();
  const wanted = savedTabs[name];
  const restored = wanted && pages.find((p) => !owned.has(p) && p.url() === wanted);
  const blank = pages.find((p) => !owned.has(p) && p.url() === 'about:blank');
  const page = restored || blank || (await browser.newPage());
  tabs.set(name, page);
  page.once('close', () => {
    if (tabs.get(name) === page) tabs.delete(name);
    // A quitting Chromium closes every page just before it disconnects; that
    // must not wipe the saved state, so only forget the tab if the browser is
    // still there a moment later.
    setTimeout(() => {
      if (!browser.connected) return;
      if (!tabs.has(name)) delete savedTabs[name];
      saveTabs();
    }, 1000);
  });
  return page;
}

// One pipeline at a time per tab; different tabs run concurrently.
function withTab(name, fn) {
  const run = (tabQueues.get(name) || Promise.resolve()).then(fn, fn);
  const tail = run.catch(() => {});
  tabQueues.set(name, tail);
  tail.then(() => {
    if (tabQueues.get(name) === tail) tabQueues.delete(name);
  });
  return run;
}

async function executePipeline(operations, tabName, close) {
  return withTab(tabName, async () => {
    const page = await getTab(tabName);
    await page.bringToFront().catch(() => {});
    const results = [];
    const images = [];
    try {
      for (const op of operations) {
        try {
          results.push({ success: true, result: await runOperation(page, op, images), operation: op.type });
        } catch (err) {
          results.push({ success: false, error: err.message || String(err), operation: op.type });
          // Capture the failure state so the caller can see what went wrong.
          try {
            images.push(await page.screenshot({ type: 'png', encoding: 'base64' }));
          } catch {}
          break;
        }
      }
    } finally {
      if (close) await page.close().catch(() => {});
      saveTabs();
    }
    return { results, images };
  });
}

const TOOLS = [
  {
    name: 'browser_execute',
    description:
      'Run browser operations as a pipeline in a tab of the shared, logged-in Chromium. ' +
      'The tab stays open between calls, so a later call continues on the same page ' +
      '(same URL, DOM and scroll position) without navigating again.\n\n' +
      'Operations:\n' +
      '- setViewport: { width, height }\n' +
      '- navigate: { url, waitUntil? } (default domcontentloaded)\n' +
      '- setContent: { html }\n' +
      '- wait: { ms } or { selector, ms? } (waits for visible element)\n' +
      '- screenshot: { fullPage? } (returned as image content)\n' +
      '- evaluate: { script } (returns result)\n' +
      '- click: { selector }\n' +
      '- type: { selector, text, delay? } (clicks to focus, then types)\n' +
      '- press: { key } (e.g. "Enter", "Control+Enter")\n' +
      '- select: { selector, values }\n' +
      '- url: {} (current url + title)\n\n' +
      'Stops on first failure and attaches a screenshot of the failing state. ' +
      'Pass a different `tab` name to work in a separate tab; pass close: true when done with one.',
    inputSchema: {
      type: 'object',
      properties: {
        operations: {
          type: 'array',
          items: {
            type: 'object',
            properties: {
              type: {
                type: 'string',
                enum: ['setViewport', 'navigate', 'setContent', 'wait', 'screenshot', 'evaluate', 'click', 'type', 'press', 'select', 'url'],
              },
              params: { type: 'object' },
            },
            required: ['type'],
          },
        },
        tab: {
          type: 'string',
          description:
            'Name of the tab to run in (default "default"). Created on first use, then reused by every call with the same name. Calls on one tab run one at a time.',
        },
        close: { type: 'boolean', description: 'Close the tab after the pipeline (default: leave it open for the next call).' },
      },
      required: ['operations'],
    },
  },
];

function text(t, isError = false) {
  return { content: [{ type: 'text', text: t }], ...(isError ? { isError: true } : {}) };
}

async function callTool(name, args = {}) {
  if (name !== 'browser_execute') return text(`Unknown tool: ${name}`, true);
  if (!Array.isArray(args.operations)) return text('Error: operations array is required', true);

  try {
    const tabName = typeof args.tab === 'string' && args.tab ? args.tab : 'default';
    const { results, images } = await executePipeline(args.operations, tabName, args.close === true);
    const failed = results.find((r) => !r.success);
    const content = [
      {
        type: 'text',
        text: failed
          ? `Pipeline failed at "${failed.operation}": ${failed.error}\n\n${JSON.stringify(results, null, 2)}`
          : `Pipeline completed (${results.length} operations)\n\n${JSON.stringify(results, null, 2)}`,
      },
      ...images.map((data) => ({ type: 'image', data, mimeType: 'image/png' })),
    ];
    return { content, ...(failed ? { isError: true } : {}) };
  } catch (err) {
    return text(`Error: ${err.message || String(err)}`, true);
  }
}

function buildServer() {
  const server = new Server({ name: 'browser', version: '2.0.0' }, { capabilities: { tools: {} } });
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: TOOLS }));
  server.setRequestHandler(CallToolRequestSchema, async (req) => callTool(req.params.name, req.params.arguments));
  return server;
}

async function readBody(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString('utf8');
  return raw ? JSON.parse(raw) : undefined;
}

// Stateless Streamable HTTP: a fresh server+transport per request.
http
  .createServer(async (req, res) => {
    if (req.url === '/healthz') {
      try {
        await getBrowser();
        res.writeHead(200).end('ok');
      } catch (err) {
        res.writeHead(503).end(String(err.message || err));
      }
      return;
    }
    if (!req.url.startsWith('/mcp')) {
      res.writeHead(404).end();
      return;
    }
    if (req.method !== 'POST') {
      res.writeHead(405, { Allow: 'POST' }).end();
      return;
    }
    try {
      const body = await readBody(req);
      const server = buildServer();
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      res.on('close', () => {
        transport.close();
        server.close();
      });
      await server.connect(transport);
      await transport.handleRequest(req, res, body);
    } catch (err) {
      if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ jsonrpc: '2.0', error: { code: -32603, message: String(err.message || err) }, id: null }));
    }
  })
  .listen(PORT, '::', () => console.log(`browser MCP listening on :${PORT}/mcp (CDP ${CDP_URL})`));
