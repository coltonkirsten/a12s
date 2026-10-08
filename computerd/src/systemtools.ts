#!/usr/bin/env node
// The a12s system tools MCP server. The harness spawns it as the agent; it forwards to computerd's unix socket.
import { request } from 'node:http';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';

const socketPath = process.env.A12S_SOCK ?? '/run/a12s/sock/computerd.sock';
const drivePath = {
  type: 'object',
  properties: {
    drive: { type: 'string', description: 'Name of a shared-write drive' },
    path: { type: 'string', description: 'Path inside the drive to lock' },
  },
  required: ['drive', 'path'],
} as const;

const sendTo = (process.env.A12S_SEND ?? '').split(',').map((x) => x.trim()).filter(Boolean);

const tools: { name: string; description: string; inputSchema: Record<string, unknown> }[] = [
  {
    name: 'restart_harness',
    description:
      'Restart your harness when this turn ends, resume this same session, and continue with a new turn. Use it to load skills or MCP servers you just wrote.',
    inputSchema: { type: 'object', properties: {} },
  },
  { name: 'lock', description: 'Take an advisory lock on a path in a shared-write drive (a lease renewed while held).', inputSchema: drivePath },
  { name: 'release', description: 'Release an advisory lock taken with lock.', inputSchema: drivePath },
];
if (sendTo.length) {
  tools.push({
    name: 'send',
    description:
      `Send a message to another Computer's inbox. You may send to: ${sendTo.join(', ')}. ` +
      'It arrives as untrusted data from you (via computer:<your name>) in their next turn; nothing is returned but a receipt. ' +
      'fresh starts a new session for it; expires (seconds, 0 = only if idle now) drops it if not delivered in time; ' +
      'replaceKey replaces your earlier pending message with the same key.',
    inputSchema: {
      type: 'object',
      properties: {
        computer: { type: 'string', description: `One of: ${sendTo.join(', ')}` },
        body: { type: 'string', description: 'The message text' },
        fresh: { type: 'boolean' },
        expires: { type: 'integer', minimum: 0 },
        replaceKey: { type: 'string' },
      },
      required: ['computer', 'body'],
    },
  });
}

function call(path: string, body: unknown): Promise<string> {
  const text = JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const req = request({ socketPath, path, method: 'POST', headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) } }, (res) => {
      let out = '';
      res.setEncoding('utf8').on('data', (d: string) => (out += d));
      res.on('end', () => (res.statusCode === 200 ? resolve(out) : reject(new Error(out || `HTTP ${res.statusCode}`))));
    });
    req.on('error', reject);
    req.end(text);
  });
}

const server = new Server({ name: 'a12s', version: '0.1.0' }, { capabilities: { tools: {} } });
server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }));
server.setRequestHandler(CallToolRequestSchema, async (req) => {
  const name = req.params.name;
  if (!tools.some((t) => t.name === name)) return { isError: true, content: [{ type: 'text', text: `unknown tool ${name}` }] };
  try {
    const args = req.params.arguments ?? {};
    const payload = name === 'restart_harness' ? {} : name === 'send' ? args : { drive: args.drive, path: args.path };
    const out = await call(`/v1/system/${name}`, payload);
    const controllerStatus = (JSON.parse(out) as { status?: unknown }).status;
    const refused = typeof controllerStatus === 'number' && controllerStatus >= 300;
    return { isError: refused, content: [{ type: 'text', text: out }] };
  } catch (err) {
    return { isError: true, content: [{ type: 'text', text: (err as Error).message }] };
  }
});
await server.connect(new StdioServerTransport());
