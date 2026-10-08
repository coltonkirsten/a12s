import type { IncomingMessage, ServerResponse } from 'node:http';
import type { TLSSocket } from 'node:tls';
import { HttpError, type Computer, type TurnRequest } from './computer.js';
import type { EventEnvelope, Journal } from './journal.js';
import type { Leases } from './leases.js';

const MAX_BODY = 2 << 20; // a 1 MiB render plus 2,000 inbox entries (enhancements §4.3)
const KEEPALIVE_MS = 15_000;
const REPLAY_PAGE = 500;
const MAX_QUEUED = 10_000;

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
  const continuation = body.continuation === true;
  const req: TurnRequest = {
    requestId: str(body, 'requestId'),
    session: continuation ? 'current' : str(body, 'session'),
    prompt: continuation ? '' : str(body, 'prompt'),
  };
  if (continuation) req.continuation = true;
  if (body.lookupOnly === true) req.lookupOnly = true;
  if (body.inbox !== undefined && body.inbox !== null) {
    if (typeof body.inbox !== 'object' || Array.isArray(body.inbox)) throw new HttpError(400, 'inbox must be an object');
    req.inbox = body.inbox as Record<string, unknown>;
  }
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

/** CA membership is not enough: only the Controller's identity (CN=controller) may drive this Computer. */
export function controllerOnly(handler: Handler): Handler {
  return (req, res) => {
    const peer = (req.socket as TLSSocket).getPeerCertificate?.();
    if (!peer || peer.subject?.CN !== 'controller') return send(res, 403, { error: 'client certificate is not the controller' });
    handler(req, res);
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
    const summary = /^\/v1\/turns\/([^/]+)$/.exec(path);
    if (summary && method === 'GET') {
      const s = journal.turnSummary(decodeURIComponent(summary[1] ?? ''));
      if (!s) throw new HttpError(404, 'unknown turn');
      return send(res, 200, s);
    }
    const stop = /^\/v1\/turns\/([^/]+)\/stop$/.exec(path);
    if (stop && method === 'POST') return send(res, 200, computer.stopTurn(decodeURIComponent(stop[1] ?? '')));
    if (path === '/v1/events' && method === 'GET') {
      const lastId = req.headers['last-event-id'];
      return streamEvents(req, res, journal, url.searchParams.get('turn'), Array.isArray(lastId) ? lastId[0] : lastId);
    }
    if (path === '/v1/status' && method === 'GET') return send(res, 200, computer.status());
    if (path === '/v1/journal/inbox' && method === 'POST') {
      const body = await readJson(req);
      if (!Array.isArray(body.records)) throw new HttpError(400, 'records must be an array');
      const records = body.records.map((r: unknown) => {
        if (typeof r !== 'object' || r === null || typeof (r as { id?: unknown }).id !== 'string') throw new HttpError(400, 'each record needs an id');
        return r as { id: string };
      });
      return send(res, 200, { journaled: journal.journalInbox(records) });
    }
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
 * When the replay is complete, `event: head` carries `{seq}`: everything up to that journal seq has been sent.
 * Writes honour backpressure; a consumer that stays undrained for more than `maxQueued` live events is
 * disconnected and resumes with Last-Event-ID.
 */
export function streamEvents(
  req: IncomingMessage,
  res: ServerResponse,
  journal: Journal,
  turn: string | null,
  lastEventId: string | undefined,
  maxQueued = MAX_QUEUED,
): void {
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
  let closed = false;
  let busy = true; // replaying or flushing; live events queue meanwhile
  const queue: EventEnvelope[] = [];
  const drained = () =>
    new Promise<void>((resolve) => {
      const done = () => (res.off('drain', done), res.off('close', done), resolve());
      res.once('drain', done);
      res.once('close', done);
    });
  /** Writes one event; resolves when the socket can take more. */
  const write = async (e: EventEnvelope) => {
    cursor = e.seq;
    if (!res.write(`id: ${e.seq}\ndata: ${JSON.stringify(e)}\n\n`)) await drained();
  };
  const flush = async () => {
    busy = true;
    for (let e = queue.shift(); e && !closed; e = queue.shift()) if (e.seq > cursor) await write(e);
    busy = false;
  };
  // Subscribe before replaying: anything appended meanwhile is queued, and seq dedupes the overlap.
  const unsubscribe = journal.subscribe((e) => {
    if (closed || (turn && e.turn !== turn)) return;
    queue.push(e);
    if (queue.length > maxQueued) return void res.destroy();
    if (!busy) void flush();
  });
  const keepalive = setInterval(() => res.write(': keepalive\n\n'), KEEPALIVE_MS);
  const close = () => {
    closed = true;
    unsubscribe();
    clearInterval(keepalive);
  };
  req.on('close', close);
  res.on('close', close);
  void (async () => {
    let snapshot: number;
    for (;;) {
      // Snapshot and page in one synchronous step: the last page then holds every match up to `snapshot`.
      snapshot = journal.lastSeq();
      const page = journal.after(cursor, turn, REPLAY_PAGE);
      for (const e of page) {
        if (closed) return;
        await write(e);
      }
      if (page.length < REPLAY_PAGE) break;
    }
    if (closed) return;
    cursor = Math.max(cursor, snapshot);
    if (!res.write(`event: head\ndata: ${JSON.stringify({ seq: snapshot })}\n\n`)) await drained();
    await flush();
  })();
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
