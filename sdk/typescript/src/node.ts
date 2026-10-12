// The Node entry ("node" export condition): connect() finds a running Fleet from the files `a12s up` writes.
import { readdir, readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";

import { A12sClient, type A12sClientOptions } from "./client.js";

export * from "./client.js";
export * from "./types.js";

export interface ConnectOptions extends A12sClientOptions {
  /** The Fleet's name (`metadata.name`). Default: found as described on {@link connect}. */
  fleet?: string;
  /** Where `.a12s/` lives. Default: the home directory. */
  home?: string;
  /** Where to start looking for fleet.yaml. Default: the current directory. */
  cwd?: string;
}

/**
 * A client for a running Fleet, as its owner. It uses the first of:
 * 1. `url` (and `token`), or `$A12S_URL` (and `$A12S_TOKEN`);
 * 2. the Fleet named by `fleet`;
 * 3. the Fleet file in `$A12S_FLEET`, else the nearest `fleet.yaml` in the current directory or a parent;
 * 4. the only Fleet with a running Controller on this machine.
 * For 2–4 it reads `~/.a12s/<fleet>/controller.json`, which `a12s up` writes.
 */
export async function connect(opts: ConnectOptions = {}): Promise<A12sClient> {
  const url = opts.url ?? process.env.A12S_URL;
  if (url) return new A12sClient({ url, token: opts.token ?? process.env.A12S_TOKEN, fetch: opts.fetch });
  const home = join(opts.home ?? homedir(), ".a12s");
  const fleet = opts.fleet ?? (await fleetNameNearby(opts.cwd ?? process.cwd())) ?? (await onlyRunningFleet(home));
  const path = join(home, fleet, "controller.json");
  let cf: { port: number; token: string };
  try {
    cf = JSON.parse(await readFile(path, "utf8"));
  } catch (err) {
    throw new Error(`Fleet ${fleet} isn't running (no ${path}): run a12s up in its directory first`, { cause: err });
  }
  return new A12sClient({ url: `http://127.0.0.1:${cf.port}`, token: cf.token, fetch: opts.fetch });
}

async function fleetNameNearby(cwd: string): Promise<string | undefined> {
  const file = process.env.A12S_FLEET ? resolve(cwd, process.env.A12S_FLEET) : await findUp(cwd, "fleet.yaml");
  if (!file) return undefined;
  const name = fleetName(await readFile(file, "utf8"));
  if (!name) throw new Error(`${file} has no metadata.name`);
  return name;
}

async function findUp(dir: string, name: string): Promise<string | undefined> {
  for (let d = resolve(dir); ; d = dirname(d)) {
    try {
      await readFile(join(d, name));
      return join(d, name);
    } catch {
      if (dirname(d) === d) return undefined;
    }
  }
}

/** metadata.name from a Fleet file, in either `metadata: { name: x }` or block form. */
export function fleetName(yaml: string): string | undefined {
  const flow = /^metadata:\s*\{[^}]*\bname:\s*["']?([A-Za-z0-9-]+)/m.exec(yaml);
  if (flow) return flow[1];
  const block = /^metadata:\s*(?:#.*)?\n(?:[ \t]+.*\n)*?[ \t]+name:\s*["']?([A-Za-z0-9-]+)/m.exec(yaml);
  return block?.[1];
}

async function onlyRunningFleet(home: string): Promise<string> {
  let names: string[] = [];
  try {
    names = await readdir(home);
  } catch {
    // no ~/.a12s yet
  }
  const running: string[] = [];
  for (const n of names) {
    try {
      await readFile(join(home, n, "controller.json"));
      running.push(n);
    } catch {
      // not running
    }
  }
  if (running.length === 1) return running[0]!;
  throw new Error(
    running.length === 0
      ? "no running Fleet found: run a12s up in your Fleet's directory, or pass connect({ url, token })"
      : `several Fleets are running (${running.join(", ")}): pass connect({ fleet: "<name>" }) or run from the Fleet's directory`,
  );
}
