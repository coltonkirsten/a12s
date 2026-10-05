import type { IncomingMessage, ServerResponse } from 'node:http';
import { HttpError, type Computer, type TurnRequest } from './computer.js';
import type { EventEnvelope, Journal } from './journal.js';
import type { Leases } from './leases.js';

const MAX_BODY = 1 << 20;
const KEEPALIVE_MS = 15_000;
const REPLAY_PAGE = 500;

type Handler = (req: IncomingMessage, res: ServerResponse) => void;

function send(res: ServerResponse, status: number, body: unknown): void {
  const text = JSON.stringify(body);
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += (chunk as Buffer).length;
    if (size > MAX_BODY) throw new HttpError(413, 'body too large');
    chunks.push(chunk as Buffer);
  }
  let v: unknown;
  try {
    v = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
  } catch {
    throw new HttpError(400, 'body is not JSON');
  }
  if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new HttpError(400, 'body must be a JSON object');
  return v as Record<string, unknown>;
}

function str(body: Record<string, unknown>, key: string): string {
  const v = body[key];
  if (typeof v !== 'string' || v === '') throw new HttpError(400, `${key} must be a non-empty string`);
  return v;
}

export function parseTurnRequest(body: Record<string, unknown>): TurnRequest {
  const req: TurnRequest = { requestId: str(body, 'requestId'), session: str(body, 'session'), prompt: str(body, 'prompt') };
  if (body.cwd !== undefined && body.cwd !== null) {
    const cwd = str(body, 'cwd');
    if (!cwd.startsWith('/')) throw new HttpError(400, 'cwd must be absolute');
    req.cwd = cwd;
  }
  return req;
}

function wrap(fn: (req: IncomingMessage, res: ServerResponse, url: URL) => Promise<void> | void): Handler {
  return (req, res) => {
    const url = new URL(req.url ?? '/', 'http://computerd');
    Promise.resolve()
      .then(() => fn(req, res, url))
      .catch((err: unknown) => {
        if (res.headersSent) return void res.destroy();
        if (err instanceof HttpError) send(res, err.status, { error: err.message });
        else send(res, 500, { error: (err as Error).message });
      });
  };
}

/** The §5 contract, served over mTLS to the Controller. */
export function apiHandler(computer: Computer, journal: Journal): Handler {
  return wrap(async (req, res, url) => {
    const path = url.pathname;
    const method = req.method ?? 'GET';
    if (path === '/v1/turns' && method === 'POST') {
      return send(res, 200, await computer.submitTurn(parseTurnRequest(await readJson(req))));
    }
    const stop = /^\/v1\/turns\/([^/]+)\/stop$/.exec(path);
    if (stop && method === 'POST') return send(res, 200, computer.stopTurn(decodeURIComponent(stop[1] ?? '')));
    if (path === '/v1/events' && method === 'GET') {
      const lastId = req.headers['last-event-id'];
      return streamEvents(req, res, journal, url.searchParams.get('turn'), Array.isArray(lastId) ? lastId[0] : lastId);
    }
    if (path === '/v1/status' && method === 'GET') return send(res, 200, computer.status());
    if (path === '/v1/diagnostics' && method === 'POST') {
      const body = await readJson(req);
      return send(res, 200, { seq: computer.diagnostic(str(body, 'message'), body.data ?? null) });
    }
    throw new HttpError(404, 'not found');
  });
}

/**
 * One SSE stream per Computer. `id:` is the journal seq. With Last-Event-ID the journal is replayed from that
 * point, then the stream goes live; a cursor the journal does not know gets `event: gap` and the stream goes live
 * from the head. Without a cursor, `?turn=` streams replay that turn from its start; unfiltered streams start live.
 */
export function streamEvents(req: IncomingMessage, res: ServerResponse, journal: Journal, turn: string | null, lastEventId: string | undefined): void {
  res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive', 'x-accel-buffering': 'no' });
  const head = journal.lastSeq();
  let cursor = turn ? 0 : head;
  if (lastEventId !== undefined) {
    const n = /^\d{1,15}$/.test(lastEventId.trim()) ? Number(lastEventId.trim()) : Number.NaN;
    if (Number.isNaN(n) || n > head) {
      res.write(`event: gap\ndata: ${JSON.stringify({ lastEventId, head })}\n\n`);
      cursor = head;
    } else cursor = n;
  }
  const write = (e: EventEnvelope) => {
    res.write(`id: ${e.seq}\ndata: ${JSON.stringify(e)}\n\n`);
    cursor = e.seq;
  };
  // Replay and subscribe in one synchronous block: appends are synchronous too, so nothing slips in between.
  for (;;) {
    const page = journal.after(cursor, turn, REPLAY_PAGE);
    page.forEach(write);
    if (page.length < REPLAY_PAGE) break;
  }
  const unsubscribe = journal.subscribe((e) => {
    if (e.seq > cursor && (!turn || e.turn === turn)) write(e);
  });
  const keepalive = setInterval(() => res.write(': keepalive\n\n'), KEEPALIVE_MS);
  const close = () => {
    unsubscribe();
    clearInterval(keepalive);
  };
  req.on('close', close);
  res.on('close', close);
}

/** System tools endpoints on the local unix socket (the agent's MCP server calls these). */
export function localHandler(computer: Computer, leases: Leases | null): Handler {
  return wrap(async (req, res, url) => {
    if (req.method !== 'POST') throw new HttpError(405, 'POST only');
    if (url.pathname === '/v1/system/restart_harness') return send(res, 200, { message: computer.requestRestart() });
    const lease = /^\/v1\/system\/(lock|release)$/.exec(url.pathname);
    if (lease) {
      if (!leases) throw new HttpError(503, 'leases are unavailable (no controller TLS)');
      const body = await readJson(req);
      const drive = str(body, 'drive');
      const path = str(body, 'path');
      const reply = lease[1] === 'lock' ? await leases.lock(drive, path) : await leases.release(drive, path);
      return send(res, 200, reply);
    }
    throw new HttpError(404, 'not found');
  });
}
