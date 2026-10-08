import { DatabaseSync } from 'node:sqlite';

export interface EventEnvelope {
  v: 1;
  computer: string;
  session: string | null;
  turn: string | null;
  seq: number;
  ts: string;
  type: string;
  data: Record<string, unknown>;
}

export interface Scope {
  turn?: string | null;
  session?: string | null;
}

interface EventRow {
  seq: number;
  turn: string | null;
  session: string | null;
  ts: string;
  type: string;
  data: string;
}

type Listener = (event: EventEnvelope) => void;

/** Append-only event journal. computerd is its only reader and writer. */
export class Journal {
  private readonly db: DatabaseSync;
  private readonly listeners = new Set<Listener>();

  constructor(path: string, private readonly computer: string) {
    this.db = new DatabaseSync(path);
    this.db.exec(`
      PRAGMA journal_mode = WAL;
      PRAGMA synchronous = FULL;
      CREATE TABLE IF NOT EXISTS events (
        seq INTEGER PRIMARY KEY, turn TEXT, session TEXT, ts TEXT NOT NULL, type TEXT NOT NULL, data JSON NOT NULL);
      CREATE INDEX IF NOT EXISTS events_turn ON events(turn, seq);
      CREATE TABLE IF NOT EXISTS turns (request_id TEXT PRIMARY KEY, turn_id TEXT NOT NULL UNIQUE, body_hash TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS refused (request_id TEXT PRIMARY KEY);
      CREATE INDEX IF NOT EXISTS events_type ON events(type, seq);
    `);
  }

  append(type: string, data: Record<string, unknown>, scope: Scope = {}): EventEnvelope {
    const event = this.insert(type, data, scope);
    this.publish(event);
    return event;
  }

  private insert(type: string, data: Record<string, unknown>, scope: Scope): EventEnvelope {
    const ts = new Date().toISOString();
    const turn = scope.turn ?? null;
    const session = scope.session ?? null;
    const r = this.db
      .prepare('INSERT INTO events (turn, session, ts, type, data) VALUES (?, ?, ?, ?, ?)')
      .run(turn, session, ts, type, JSON.stringify(data));
    return { v: 1, computer: this.computer, session, turn, seq: Number(r.lastInsertRowid), ts, type, data };
  }

  private publish(event: EventEnvelope): void {
    for (const l of this.listeners) l(event);
  }

  /** Records a turn's requestId and its turn.started event atomically. */
  acceptTurn(requestId: string, turnId: string, bodyHash: string, started: Record<string, unknown>, session: string | null): EventEnvelope {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.db.prepare('INSERT INTO turns (request_id, turn_id, body_hash) VALUES (?, ?, ?)').run(requestId, turnId, bodyHash);
      const event = this.insert('turn.started', started, { turn: turnId, session });
      this.db.exec('COMMIT');
      this.publish(event);
      return event;
    } catch (err) {
      this.db.exec('ROLLBACK');
      throw err;
    }
  }

  findRequest(requestId: string): { turnId: string; bodyHash: string } | undefined {
    const row = this.db.prepare('SELECT turn_id, body_hash FROM turns WHERE request_id = ?').get(requestId) as
      | { turn_id: string; body_hash: string }
      | undefined;
    return row && { turnId: row.turn_id, bodyHash: row.body_hash };
  }

  /** The first and last events of a turn, for idempotent replies and stop. */
  turnInfo(turnId: string): { session: string | null; finished: string | null } | undefined {
    if (!this.db.prepare("SELECT 1 FROM events WHERE turn = ? AND type = 'turn.started' LIMIT 1").get(turnId)) return undefined;
    // A new session's id is known only after dispatch (session.changed), so take the first one the turn recorded.
    const started = this.db.prepare('SELECT session FROM events WHERE turn = ? AND session IS NOT NULL ORDER BY seq LIMIT 1').get(turnId) as
      | { session: string }
      | undefined;
    const fin = this.db.prepare("SELECT data FROM events WHERE turn = ? AND type = 'turn.finished' LIMIT 1").get(turnId) as
      | { data: string }
      | undefined;
    return { session: started?.session ?? null, finished: fin ? String(JSON.parse(fin.data).status) : null };
  }

  /** Accepted turns with no turn.finished: computerd died while they ran. */
  unfinishedTurns(): { turnId: string; session: string | null }[] {
    const rows = this.db
      .prepare(
        `SELECT t.turn_id AS turnId FROM turns t WHERE NOT EXISTS
           (SELECT 1 FROM events e WHERE e.turn = t.turn_id AND e.type = 'turn.finished') ORDER BY t.rowid`,
      )
      .all() as { turnId: string }[];
    return rows.map((r) => ({ turnId: r.turnId, session: this.turnInfo(r.turnId)?.session ?? null }));
  }

  /** Tombstones a requestId a lookup-only replay did not find, journaling turn.refused (§4.11). */
  refuse(requestId: string): void {
    this.db.exec('BEGIN IMMEDIATE');
    let event: EventEnvelope;
    try {
      this.db.prepare('INSERT OR IGNORE INTO refused (request_id) VALUES (?)').run(requestId);
      event = this.insert('turn.refused', { requestId }, {});
      this.db.exec('COMMIT');
    } catch (err) {
      this.db.exec('ROLLBACK');
      throw err;
    }
    this.publish(event);
  }

  isRefused(requestId: string): boolean {
    return this.db.prepare('SELECT 1 FROM refused WHERE request_id = ?').get(requestId) !== undefined;
  }

  /** The restart_harness continuation still owed: journaled, neither dispatched nor abandoned. */
  owedContinuation(): { id: string; parentTurn: string | null } | null {
    const row = this.db
      .prepare("SELECT data FROM events WHERE type = 'harness.restarted' ORDER BY seq DESC LIMIT 1")
      .get() as { data: string } | undefined;
    if (!row) return null;
    const d = JSON.parse(row.data) as { continuation?: string | null; parentTurn?: string | null };
    if (!d.continuation) return null;
    const done = this.db
      .prepare(
        `SELECT 1 FROM events WHERE (type = 'turn.dispatched' OR type = 'continuation.abandoned')
           AND json_extract(data, '$.continuation') = ? LIMIT 1`,
      )
      .get(d.continuation);
    return done ? null : { id: d.continuation, parentTurn: d.parentTurn ?? null };
  }

  /** What settlement needs to know about one turn (§4.6). */
  turnSummary(turnId: string): Record<string, unknown> | undefined {
    const req = this.db.prepare('SELECT request_id FROM turns WHERE turn_id = ?').get(turnId) as { request_id: string } | undefined;
    if (!req) return undefined;
    const has = (type: string) => this.db.prepare('SELECT 1 FROM events WHERE turn = ? AND type = ? LIMIT 1').get(turnId, type) !== undefined;
    const fin = this.db.prepare("SELECT data FROM events WHERE turn = ? AND type = 'turn.finished' LIMIT 1").get(turnId) as { data: string } | undefined;
    const finished = fin ? (JSON.parse(fin.data) as { status?: string; error?: string }) : null;
    return {
      turnId,
      requestId: req.request_id,
      dispatched: has('turn.dispatched'),
      resumeFailed: has('resume.failed'),
      finished: finished?.status ?? null,
      error: finished?.error ?? null,
      session: this.turnInfo(turnId)?.session ?? null,
    };
  }

  lastSeq(): number {
    const row = this.db.prepare('SELECT MAX(seq) AS m FROM events').get() as { m: number | null };
    return row.m ?? 0;
  }

  /** Events with seq > after, oldest first. */
  after(after: number, turn: string | null, limit: number): EventEnvelope[] {
    const rows = (
      turn
        ? this.db.prepare('SELECT * FROM events WHERE seq > ? AND turn = ? ORDER BY seq LIMIT ?').all(after, turn, limit)
        : this.db.prepare('SELECT * FROM events WHERE seq > ? ORDER BY seq LIMIT ?').all(after, limit)
    ) as unknown as EventRow[];
    return rows.map((r) => ({
      v: 1,
      computer: this.computer,
      session: r.session,
      turn: r.turn,
      seq: r.seq,
      ts: r.ts,
      type: r.type,
      data: JSON.parse(r.data) as Record<string, unknown>,
    }));
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  close(): void {
    this.db.close();
  }
}
