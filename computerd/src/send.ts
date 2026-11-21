import { randomBytes } from 'node:crypto';
import { request } from 'node:https';
import type { Config } from './config.js';

export interface TlsMaterial {
  ca: Buffer;
  cert: Buffer;
  key: Buffer;
}

export interface SendRequest {
  computer: string;
  body: string;
  fresh?: boolean;
  expires?: number;
  replaceKey?: string;
}

export interface SendReply {
  status: number;
  body: unknown;
}

/**
 * The `send` system tool: computerd enqueues to another Computer's inbox over its own mTLS
 * identity; the Controller checks the grant and sets via = computer:<name>. The agent holds no credential.
 */
export function sendToInbox(cfg: Config, tls: TlsMaterial, req: SendRequest): Promise<SendReply> {
  if (!/^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/.test(req.computer)) return Promise.resolve({ status: 400, body: { error: 'bad computer name' } });
  const payload: Record<string, unknown> = {
    body: req.body,
    sender: `agent:${cfg.computer}`,
    idempotencyKey: `send-${randomBytes(12).toString('hex')}`,
  };
  if (req.fresh) payload.fresh = true;
  if (typeof req.expires === 'number') payload.expires = req.expires;
  if (req.replaceKey) payload.replaceKey = req.replaceKey;
  return controllerCall(cfg, tls, 'POST', `/v1/agent-computers/${req.computer}/inbox`, payload);
}

export interface ControllerReply extends SendReply {
  etag: string | null;
}

/** One call to the Controller's mTLS listener as this Computer. A non-JSON body comes back as text. */
export function controllerCall(cfg: Config, tls: TlsMaterial, method: string, path: string, payload?: unknown, headers: Record<string, string> = {}): Promise<ControllerReply> {
  if (!cfg.controller) return Promise.reject(new Error('A12S_CONTROLLER is not set'));
  const text = payload === undefined ? '' : JSON.stringify(payload);
  const url = new URL(path, cfg.controller);
  const h: Record<string, string> = { ...headers, 'content-length': String(Buffer.byteLength(text)) };
  if (payload !== undefined) h['content-type'] = 'application/json';
  return new Promise((resolve, reject) => {
    const r = request(url, { method, ...tls, headers: h, timeout: 15_000 }, (res) => {
      let out = '';
      res.setEncoding('utf8').on('data', (d: string) => (out += d));
      res.on('end', () => {
        let parsed: unknown = out;
        try {
          parsed = JSON.parse(out);
        } catch {
          // keep the raw text
        }
        const etag = res.headers.etag;
        resolve({ status: res.statusCode ?? 0, body: parsed, etag: typeof etag === 'string' ? etag : null });
      });
    });
    r.on('timeout', () => r.destroy(new Error('controller timed out')));
    r.on('error', reject);
    r.end(text);
  });
}
