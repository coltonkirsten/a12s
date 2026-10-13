import { randomBytes } from 'node:crypto';
import { request } from 'node:https';
import type { Config } from './config.js';
import type { TlsMaterial } from './leases.js';

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
  if (!cfg.controller) return Promise.reject(new Error('A12S_CONTROLLER is not set'));
  if (!/^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/.test(req.computer)) return Promise.resolve({ status: 400, body: { error: 'bad computer name' } });
  const payload: Record<string, unknown> = {
    body: req.body,
    sender: `agent:${cfg.computer}`,
    idempotencyKey: `send-${randomBytes(12).toString('hex')}`,
  };
  if (req.fresh) payload.fresh = true;
  if (typeof req.expires === 'number') payload.expires = req.expires;
  if (req.replaceKey) payload.replaceKey = req.replaceKey;
  const text = JSON.stringify(payload);
  const url = new URL(`/v1/agent-computers/${req.computer}/inbox`, cfg.controller);
  return new Promise((resolve, reject) => {
    const r = request(
      url,
      { method: 'POST', ...tls, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) }, timeout: 15_000 },
      (res) => {
        let out = '';
        res.setEncoding('utf8').on('data', (d: string) => (out += d));
        res.on('end', () => {
          let parsed: unknown = out;
          try {
            parsed = JSON.parse(out);
          } catch {
            // keep the raw text
          }
          resolve({ status: res.statusCode ?? 0, body: parsed });
        });
      },
    );
    r.on('timeout', () => r.destroy(new Error('controller timed out')));
    r.on('error', reject);
    r.end(text);
  });
}
