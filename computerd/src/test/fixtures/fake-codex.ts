#!/usr/bin/env node
// A stand-in for `codex app-server`: newline-delimited JSON-RPC on stdio. Mode comes from FAKE_MODE.
import { appendFileSync } from 'node:fs';
import { createInterface } from 'node:readline';

const mode = process.env.FAKE_MODE ?? 'normal';
const log = (m: unknown) => process.env.FAKE_LOG && appendFileSync(process.env.FAKE_LOG, `${JSON.stringify(m)}\n`);
log({ argv: process.argv.slice(2) });
const send = (m: unknown) => process.stdout.write(`${JSON.stringify(m)}\n`);
const notify = (method: string, params: unknown) => send({ method, params });
let threads = 0;
let current: { threadId: string; turnId: string } | null = null;

function runTurn(threadId: string, turnId: string, prompt: string): void {
  notify('turn/started', { threadId, turn: { id: turnId, status: 'inProgress', items: [] } });
  if (mode === 'hang' || mode === 'ignore-interrupt') return;
  if (prompt === 'bg') {
    // A command still running when the turn ends stays a background terminal; its completion names the old turn.
    const item = { type: 'commandExecution', id: `item_bg_${turnId}`, command: 'sleep 1; make', cwd: '/', status: 'inProgress' };
    notify('item/started', { threadId, turnId, item });
    notify('turn/completed', { threadId, turn: { id: turnId, status: process.env.FAKE_BG_STATUS ?? 'completed', items: [] } });
    current = null;
    setTimeout(() => notify('item/completed', { threadId, turnId, item: { ...item, status: 'completed', exitCode: 0 } }), Number(process.env.FAKE_BG_MS ?? 200));
    return;
  }
  notify('item/started', { threadId, turnId, item: { type: 'commandExecution', id: 'item_1', command: 'ls', cwd: '/', status: 'inProgress' } });
  send({ id: 'srv_1', method: 'item/commandExecution/requestApproval', params: { threadId, turnId, itemId: 'item_1', command: 'ls', cwd: '/' } });
}

function finishTurn(threadId: string, turnId: string): void {
  notify('item/completed', { threadId, turnId, item: { type: 'commandExecution', id: 'item_1', command: 'ls', cwd: '/', status: 'declined', exitCode: null } });
  for (const delta of ['po', 'ng']) notify('item/agentMessage/delta', { threadId, turnId, itemId: 'msg_1', delta });
  notify('thread/tokenUsage/updated', { threadId, turnId, tokenUsage: { last: { totalTokens: 5 }, total: { totalTokens: 9 } } });
  notify('turn/completed', { threadId, turn: { id: turnId, status: 'completed', items: [] } });
  current = null;
}

createInterface({ input: process.stdin }).on('line', (line) => {
  const m = JSON.parse(line) as { id?: number | string; method?: string; params?: Record<string, any>; result?: unknown };
  log(m);
  if (m.method === undefined) {
    if (m.id === 'srv_1' && current) finishTurn(current.threadId, current.turnId);
    return;
  }
  switch (m.method) {
    case 'initialize':
      return send({ id: m.id, result: { userAgent: 'fake' } });
    case 'initialized':
      return;
    case 'thread/start':
      return send({ id: m.id, result: { thread: { id: `thr_${++threads}` } } });
    case 'thread/resume':
      if (m.params?.threadId === 'thr_missing') return send({ id: m.id, error: { code: -32600, message: 'no rollout found for thread id thr_missing' } });
      return send({ id: m.id, result: { thread: { id: m.params?.threadId } } });
    case 'turn/start': {
      if (mode === 'stall') return; // never answers
      const turnId = `turn_${Date.now()}`;
      current = { threadId: String(m.params?.threadId), turnId };
      send({ id: m.id, result: { turn: { id: turnId, status: 'inProgress', items: [] } } });
      return runTurn(current.threadId, turnId, String(m.params?.input?.[0]?.text ?? ''));
    }
    case 'turn/interrupt':
      send({ id: m.id, result: {} });
      if (mode !== 'ignore-interrupt') notify('turn/completed', { threadId: m.params?.threadId, turn: { id: m.params?.turnId, status: 'interrupted', items: [] } });
      return;
  }
});
