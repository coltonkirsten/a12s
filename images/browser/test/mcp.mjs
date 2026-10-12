// Minimal MCP stdio client: launch Playwright MCP exactly as computerd would, navigate to a data: URL.
// Exits 1 unless the page's text comes back (the tiers step of scripts/check.sh).
setTimeout(() => { console.error('FAIL: timed out'); process.exit(1); }, 60_000).unref();
import { spawn } from 'node:child_process';
const p = spawn('node', ['/opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js', '--config', '/opt/a12s/playwright-mcp/config.json'], { stdio: ['pipe', 'pipe', 'inherit'] });
let buf = '', waiters = {};
p.stdout.on('data', d => { buf += d; let i; while ((i = buf.indexOf('\n')) >= 0) { const m = JSON.parse(buf.slice(0, i)); buf = buf.slice(i + 1); if (m.id && waiters[m.id]) waiters[m.id](m); } });
let id = 0;
const call = (method, params) => new Promise(r => { const i = ++id; waiters[i] = r; p.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: i, method, params }) + '\n'); });
await call('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'a12s-test', version: '0' } });
p.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
const nav = await call('tools/call', { name: 'browser_navigate', arguments: { url: 'data:text/html,<title>a12s</title><h1>hello from playwright mcp</h1>' } });
const out = JSON.stringify(nav.result ?? nav.error);
console.log(out.slice(0, 600));
const ok = !nav.error && !nav.result?.isError && out.includes('hello from playwright mcp');
await call('tools/call', { name: 'browser_close', arguments: {} });
p.kill();
if (!ok) console.error('FAIL: navigation did not return the page');
process.exit(ok ? 0 : 1);
