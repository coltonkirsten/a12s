import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { createServer, request } from 'node:https';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { test } from 'node:test';
import { controllerOnly } from '../server.js';
import { testConfig } from './helpers.js';

test('the control API accepts only the controller identity, even among CA-signed certs', async () => {
  const dir = testConfig().runDir;
  const p = (f: string) => join(dir, f);
  const ssl = (...args: string[]) => execFileSync('openssl', args, { stdio: ['ignore', 'ignore', 'pipe'] });
  const ec = ['-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes'];
  ssl('req', '-x509', ...ec, '-keyout', p('ca.key'), '-out', p('ca.crt'), '-subj', '/CN=test-ca', '-days', '1');
  const issue = (cn: string, ext: string) => {
    writeFileSync(p(`${cn}.ext`), ext);
    ssl('req', ...ec, '-keyout', p(`${cn}.key`), '-out', p(`${cn}.csr`), '-subj', `/CN=${cn}`);
    ssl('x509', '-req', '-in', p(`${cn}.csr`), '-CA', p('ca.crt'), '-CAkey', p('ca.key'), '-CAcreateserial', '-out', p(`${cn}.crt`), '-days', '1', '-extfile', p(`${cn}.ext`));
  };
  issue('localhost', 'subjectAltName=DNS:localhost\nextendedKeyUsage=serverAuth\n');
  issue('controller', 'extendedKeyUsage=clientAuth\n');
  issue('scout', 'extendedKeyUsage=clientAuth\n'); // e.g. another Computer's cert
  const ca = readFileSync(p('ca.crt'));
  const server = createServer(
    { ca, cert: readFileSync(p('localhost.crt')), key: readFileSync(p('localhost.key')), requestCert: true, rejectUnauthorized: true },
    controllerOnly((_req, res) => res.end('{"phase":"ready"}')),
  );
  // An explicit address: 'localhost' may bind ::1 while the client resolves 127.0.0.1 (seen on Linux).
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const port = (server.address() as AddressInfo).port;
  const get = (cn: string) =>
    new Promise<number>((resolve, reject) => {
      const req = request({ host: '127.0.0.1', servername: 'localhost', port, path: '/v1/status', ca, cert: readFileSync(p(`${cn}.crt`)), key: readFileSync(p(`${cn}.key`)) }, (res) => {
        res.resume();
        resolve(res.statusCode ?? 0);
      });
      req.on('error', reject);
      req.end();
    });
  try {
    assert.equal(await get('controller'), 200);
    assert.equal(await get('scout'), 403);
  } finally {
    server.close();
  }
});
