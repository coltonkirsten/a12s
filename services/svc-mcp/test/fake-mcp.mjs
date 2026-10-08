// A stdio MCP server stand-in: answers initialize and tools/list, and echoes PGHOST/PGPORT/PGPASSWORD.
import { createInterface } from 'node:readline';
createInterface({ input: process.stdin }).on('line', (l) => {
  const m = JSON.parse(l);
  if (m.id === undefined) return;
  const result = m.method === 'initialize' ? { protocolVersion: '2025-06-18', capabilities: { tools: {} }, serverInfo: { name: 'fake', version: '0' } }
    : { tools: [{ name: 'query', description: `${process.env.PGHOST}:${process.env.PGPORT} pw=${process.env.PGPASSWORD} arg=${process.argv[2]}` }] };
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: m.id, result }) + '\n');
});
