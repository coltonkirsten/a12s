#!/usr/bin/env node
// A stand-in for the Claude Code binary speaking the SDK's stream-json protocol. Mode comes from FAKE_MODE.
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
let n = 0;
const out = (m: Record<string, unknown>) => process.stdout.write(`${JSON.stringify({ uuid: `u${n++}`, session_id: sessionId, ...m })}\n`);
// wrong-session: the init frame names another session, as if the resume had silently started fresh.
const result = (subtype: string, isError: boolean, text: string) =>
  out({
    type: 'result', subtype, is_error: isError, result: text, errors: isError ? ['interrupted'] : [], stop_reason: 'end_turn',
    usage: { input_tokens: 3, output_tokens: 2, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    total_cost_usd: 0.001, duration_ms: 5, duration_api_ms: 4, num_turns: 1, modelUsage: {},
    permission_denials: mode === 'deny' ? [{ tool_name: 'Bash', tool_use_id: 'toolu_2', tool_input: {} }] : [],
  });

process.on('SIGTERM', () => {
  if (mode !== 'ignore-term') process.exit(143);
});

createInterface({ input: process.stdin }).on('line', (line) => {
  const m = JSON.parse(line) as { type: string; request_id?: string; request?: { subtype: string } };
  if (m.type === 'control_request') {
    process.stdout.write(`${JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: m.request_id, response: {} } })}\n`);
    if (m.request?.subtype === 'interrupt' && (mode === 'hang' || mode === 'wrong-session')) result('error_during_execution', true, '');
    return;
  }
  if (m.type !== 'user') return;
  out({ ...(mode === 'wrong-session' ? { session_id: 'aaaaaaaa-0000-4000-8000-000000000000' } : {}), type: 'system', subtype: 'init', cwd: process.cwd(), tools: [], mcp_servers: [], model: 'fake', permissionMode: 'default', slash_commands: [], apiKeySource: 'none', claude_code_version: 'fake' });
  if (mode === 'hang' || mode === 'ignore-term' || mode === 'wrong-session') return;
  if (mode === 'refresh') {
    // Like a real token refresh: write a temp file and rename it over the credentials symlink.
    const file = `${process.env.CLAUDE_CONFIG_DIR}/.credentials.json`;
    writeFileSync(`${file}.tmp`, '{"claudeAiOauth":{"accessToken":"refreshed"}}');
    renameSync(`${file}.tmp`, file);
  }
  for (const text of ['po', 'ng']) {
    out({ type: 'stream_event', parent_tool_use_id: null, event: { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text } } });
  }
  out({ type: 'assistant', parent_tool_use_id: null, message: { role: 'assistant', content: [{ type: 'tool_use', id: 'toolu_1', name: 'Bash', input: { command: 'echo hi' } }] } });
  out({ type: 'user', parent_tool_use_id: null, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_1', content: 'hi', is_error: false }] } });
  if (mode === 'deny') out({ type: 'system', subtype: 'permission_denied', tool_name: 'Bash', tool_use_id: 'toolu_3', decision_reason_type: 'mode' });
  result('success', false, 'pong');
});
process.stdin.on('end', () => process.exit(0));
