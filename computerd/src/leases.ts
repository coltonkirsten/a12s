import { request } from 'node:https';
import type { Config } from './config.js';

export interface TlsMaterial {
  ca: Buffer;
  cert: Buffer;
  key: Buffer;
}

export interface LeaseReply {
  status: number;
  body: unknown;
}

const TTL_SECONDS = 60;
const RENEW_MS = 30_000;

/** Advisory locks held as Controller leases (`POST/DELETE /v1/leases`), renewed while held. */
export class Leases {
  private readonly held = new Map<string, NodeJS.Timeout>();

  constructor(
    private readonly cfg: Config,
    private readonly tls: TlsMaterial,
    private readonly onLost: (drive: string, path: string, reply: LeaseReply | Error) => void,
  ) {}

  async lock(drive: string, path: string): Promise<LeaseReply> {
    const reply = await this.call('POST', drive, path);
    const key = JSON.stringify([drive, path]);
    if (reply.status >= 200 && reply.status < 300 && !this.held.has(key)) {
      const timer = setInterval(() => {
        this.call('POST', drive, path).then(
          (r) => r.status >= 300 && this.lost(key, drive, path, r),
          (err: Error) => this.lost(key, drive, path, err),
        );
      }, RENEW_MS);
      timer.unref();
      this.held.set(key, timer);
    }
    return reply;
  }

  async release(drive: string, path: string): Promise<LeaseReply> {
    const key = JSON.stringify([drive, path]);
    clearInterval(this.held.get(key));
    this.held.delete(key);
    return this.call('DELETE', drive, path);
  }

  close(): void {
    for (const t of this.held.values()) clearInterval(t);
    this.held.clear();
  }

  private lost(key: string, drive: string, path: string, why: LeaseReply | Error): void {
    clearInterval(this.held.get(key));
    this.held.delete(key);
    this.onLost(drive, path, why);
  }

  private call(method: 'POST' | 'DELETE', drive: string, path: string): Promise<LeaseReply> {
    if (!this.cfg.controller) return Promise.reject(new Error('A12S_CONTROLLER is not set'));
    const body = JSON.stringify({ computer: this.cfg.computer, drive, path, ttl: TTL_SECONDS });
    const url = new URL('/v1/leases', this.cfg.controller);
    return new Promise((resolve, reject) => {
      const req = request(
        url,
        { method, ...this.tls, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) }, timeout: 10_000 },
        (res) => {
          let text = '';
          res.setEncoding('utf8').on('data', (d: string) => (text += d));
          res.on('end', () => {
            let parsed: unknown = text;
            try {
              parsed = JSON.parse(text);
            } catch {
              // keep the raw text
            }
            resolve({ status: res.statusCode ?? 0, body: parsed });
          });
        },
      );
      req.on('timeout', () => req.destroy(new Error('controller timed out')));
      req.on('error', reject);
      req.end(body);
    });
  }
}
