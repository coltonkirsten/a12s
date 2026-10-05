// Minimal MCP stdio client: launch Playwright MCP exactly as computerd would, navigate to a data: URL.
import { spawn } from 'node:child_process';
const p = spawn('node', ['/opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js', '--config', '/opt/a12s/playwright-mcp/config.json'], { stdio: ['pipe', 'pipe', 'inherit'] });
let buf = '', waiters = {};
p.stdout.on('data', d => { buf += d; let i; while ((i = buf.indexOf('\n')) >= 0) { const m = JSON.parse(buf.slice(0, i)); buf = buf.slice(i + 1); if (m.id && waiters[m.id]) waiters[m.id](m); } });
let id = 0;
const call = (method, params) => new Promise(r => { const i = ++id; waiters[i] = r; p.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: i, method, params }) + '\n'); });
await call('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'a12s-test', version: '0' } });
p.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
const nav = await call('tools/call', { name: 'browser_navigate', arguments: { url: 'data:text/html,<title>a12s</title><h1>hello from playwright mcp</h1>' } });
console.log(JSON.stringify(nav.result ?? nav.error).slice(0, 600));
await call('tools/call', { name: 'browser_close', arguments: {} });
p.kill(); process.exit(0);
