#!/usr/bin/env node
// A stand-in for the Claude Code binary speaking the SDK's stream-json protocol, one long-lived process per session
// like the real CLI in streaming-input mode. Mode comes from FAKE_MODE; a prompt can steer one turn:
//   `bg <ms>`    launches a background task that ends after <ms>, and then starts a turn by itself
//   `slow <ms>`  the turn takes <ms> before its result
//   `crash`      the process dies mid-turn, after receipt
// FAKE_SELF_MS keeps a self-started turn open that long before its result. FAKE_ORPHAN makes a resumed process
// report a task the previous one left behind, in a turn without a model call, as the real CLI does. FAKE_DONE_FILE
// gets a line for each prompt turn that ran to completion. FAKE_NO_LEVEL leaves out background_tasks_changed, like an
// older CLI; FAKE_LOST_END leaves out a finished task's notification, so only the live set says it ended.
import { appendFileSync, renameSync, writeFileSync } from 'node:fs';
import { createInterface } from 'node:readline';

const mode = process.env.FAKE_MODE ?? 'normal';
const args = process.argv.slice(2);
if (process.env.FAKE_ARGS_FILE) appendFileSync(process.env.FAKE_ARGS_FILE, `${JSON.stringify(args)}\n`);
const flag = (name: string) => {
  const eq = args.find((a) => a.startsWith(`${name}=`));
  if (eq) return eq.slice(name.length + 1);
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const sessionId = flag('--resume') ?? flag('--session-id') ?? 'no-session';
const lifecycle = mode !== 'no-lifecycle';
let n = 0;
const out = (m: Record<string, unknown>) => process.stdout.write(`${JSON.stringify({ uuid: `u${n++}`, session_id: sessionId, ...m })}\n`);
const result = (subtype: string, isError: boolean, text: string) =>
  out({
    type: 'result', subtype, is_error: isError, result: text, errors: isError ? ['interrupted'] : [], stop_reason: 'end_turn',
    usage: { input_tokens: 3, output_tokens: 2, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    total_cost_usd: 0.001, duration_ms: 5, duration_api_ms: 4, num_turns: 1, modelUsage: {},
    permission_denials: mode === 'deny' ? [{ tool_name: 'Bash', tool_use_id: 'toolu_2', tool_input: {} }] : [],
  });
// wrong-session: the init frame names another session, as if the resume had silently started fresh.
const init = () =>
  out({ ...(mode === 'wrong-session' ? { session_id: 'aaaaaaaa-0000-4000-8000-000000000000' } : {}), type: 'system', subtype: 'init', cwd: process.cwd(), tools: [], mcp_servers: [], model: 'fake', permissionMode: 'default', slash_commands: [], apiKeySource: 'none', claude_code_version: 'fake' });
const text = (t: string) => out({ type: 'stream_event', parent_tool_use_id: null, event: { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: t } } });
const command = (uuid: string, state: string) => lifecycle && out({ type: 'command_lifecycle', command_uuid: uuid, state });

interface Prompt {
  uuid: string;
  message: { content?: unknown };
}
type Work = { kind: 'prompt'; prompt: Prompt } | { kind: 'notification'; task: string };
const queue: Work[] = [];
let running: { uuid: string | null; end: () => void } | null = null;
let eof = false;
const tasks = new Map<string, NodeJS.Timeout>();
let taskSeq = 0;
const level = () => !process.env.FAKE_NO_LEVEL && out({ type: 'system', subtype: 'background_tasks_changed', tasks: [...tasks.keys()].map((task_id) => ({ task_id, task_type: 'local_agent', description: task_id })) });

process.on('SIGTERM', () => {
  if (mode !== 'ignore-term') process.exit(143);
});

function next(): void {
  if (running) return;
  const work = queue.shift();
  if (!work) {
    if (eof) shutdown();
    return;
  }
  if (work.kind === 'notification') return selfTurn(work.task);
  promptTurn(work.prompt);
}

function finishTurn(subtype = 'success', isError = false, reply = 'pong'): void {
  const r = running;
  running = null;
  result(subtype, isError, reply);
  if (r?.uuid) command(r.uuid, isError ? 'cancelled' : 'completed');
  if (r?.uuid && !isError && process.env.FAKE_DONE_FILE) appendFileSync(process.env.FAKE_DONE_FILE, `${r.uuid}\n`);
  setImmediate(next);
}

/** The CLI starts a turn by itself after a background task's notification: init, no prompt, no echo. */
function selfTurn(task: string): void {
  running = { uuid: null, end: () => finishTurn('error_during_execution', true, '') };
  init();
  text(`task ${task} finished`);
  out({ type: 'assistant', parent_tool_use_id: null, message: { role: 'assistant', content: [{ type: 'text', text: `task ${task} finished` }] } });
  const done = () => running?.uuid === null && finishTurn('success', false, `task ${task} finished`);
  if (process.env.FAKE_SELF_MS) setTimeout(done, Number(process.env.FAKE_SELF_MS));
  else done();
}

function promptTurn(m: Prompt): void {
  const body = typeof m.message?.content === 'string' ? m.message.content : '';
  running = { uuid: m.uuid, end: () => finishTurn('error_during_execution', true, '') };
  command(m.uuid, 'started');
  // --replay-user-messages: the CLI echoes the user message it took in (the receipt behind turn.dispatched).
  const replay = () => {
    if (args.includes('--replay-user-messages') && mode !== 'no-replay') out({ type: 'user', isReplay: true, uuid: m.uuid, parent_tool_use_id: null, message: m.message });
  };
  if (mode === 'replay-first') replay();
  init();
  if (mode !== 'replay-first') replay();
  if (mode === 'hang' || mode === 'ignore-term' || mode === 'wrong-session') return;
  if (body === 'crash') process.exit(3);
  const bg = /^bg (\d+)$/.exec(body);
  if (bg) {
    const id = `task_${++taskSeq}`;
    out({ type: 'assistant', parent_tool_use_id: null, message: { role: 'assistant', content: [{ type: 'tool_use', id: `toolu_${id}`, name: 'Agent', input: { run_in_background: true } }] } });
    out({ type: 'system', subtype: 'task_started', task_id: id, tool_use_id: `toolu_${id}`, description: `sleep ${bg[1]}`, task_type: 'local_agent', is_backgrounded: true });
    out({ type: 'user', parent_tool_use_id: null, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: `toolu_${id}`, content: 'Async agent launched', is_error: false }] } });
    tasks.set(
      id,
      setTimeout(() => {
        tasks.delete(id);
        level();
        // Sub-agent output between turns belongs to no turn.
        out({ type: 'assistant', parent_tool_use_id: `toolu_${id}`, message: { role: 'assistant', content: [{ type: 'tool_use', id: `toolu_sub_${id}`, name: 'Bash', input: {} }] } });
        out({ type: 'stream_event', parent_tool_use_id: `toolu_${id}`, event: { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'sub-agent text' } } });
        if (!process.env.FAKE_LOST_END) out({ type: 'system', subtype: 'task_notification', task_id: id, status: 'completed', output_file: '', summary: 'done' });
        queue.push({ kind: 'notification', task: id });
        next();
      }, Number(bg[1])),
    );
    level();
    text('launched');
    return finishTurn('success', false, 'launched');
  }
  const slow = /^slow (\d+)$/.exec(body);
  if (slow) {
    text('slow');
    const turn = running;
    return void setTimeout(() => running === turn && finishTurn(), Number(slow[1]));
  }
  if (mode === 'refresh') {
    // Like a real token refresh: write a temp file and rename it over the credentials symlink.
    const file = `${process.env.CLAUDE_CONFIG_DIR}/.credentials.json`;
    writeFileSync(`${file}.tmp`, '{"claudeAiOauth":{"accessToken":"refreshed"}}');
    renameSync(`${file}.tmp`, file);
    if (process.env.FAKE_HOLD_MS) {
      // Keep the turn open so a test can see the reclaim happen mid-turn.
      return void setTimeout(() => finishTurn(), Number(process.env.FAKE_HOLD_MS));
    }
  }
  for (const t of ['po', 'ng']) text(t);
  out({ type: 'assistant', parent_tool_use_id: null, message: { role: 'assistant', content: [{ type: 'tool_use', id: 'toolu_1', name: 'Bash', input: { command: 'echo hi' } }] } });
  out({ type: 'user', parent_tool_use_id: null, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_1', content: 'hi', is_error: false }] } });
  if (mode === 'deny') out({ type: 'system', subtype: 'permission_denied', tool_name: 'Bash', tool_use_id: 'toolu_3', decision_reason_type: 'mode' });
  finishTurn();
}

createInterface({ input: process.stdin }).on('line', (line) => {
  const m = JSON.parse(line) as { type: string; request_id?: string; request?: { subtype: string }; uuid?: string; message?: { content?: unknown } };
  if (m.type === 'control_request') {
    process.stdout.write(`${JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: m.request_id, response: {} } })}\n`);
    // An interrupt ends the running turn; background tasks keep running.
    if (m.request?.subtype === 'interrupt' && running && mode !== 'ignore-term') running.end();
    return;
  }
  if (m.type !== 'user') return;
  const prompt = { uuid: m.uuid ?? `p${n}`, message: m.message ?? {} };
  if (process.env.FAKE_ORPHAN && flag('--resume') && n === 0) {
    out({ type: 'system', subtype: 'task_notification', task_id: 'orphan', status: 'stopped', output_file: '', summary: 'did not finish before the previous session ended' });
    init();
    out({ type: 'result', subtype: 'success', is_error: false, result: '', errors: [], usage: { input_tokens: 0, output_tokens: 0 }, total_cost_usd: 0, duration_ms: 10, num_turns: 0, permission_denials: [] });
  }
  command(prompt.uuid, 'queued');
  queue.push({ kind: 'prompt', prompt });
  next();
});
// Like the real CLI, end of input lets the running and queued prompts finish, then stops the background tasks and exits.
process.stdin.on('end', () => {
  eof = true;
  if (!running && !queue.length) shutdown();
});

function shutdown(): void {
  for (const [id, timer] of tasks) {
    clearTimeout(timer);
    out({ type: 'system', subtype: 'task_notification', task_id: id, status: 'stopped', output_file: '', summary: '' });
  }
  process.exit(0);
}
