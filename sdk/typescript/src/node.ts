// Node-only helpers: "@coltonkirsten/a12s-client/node". Everything in the root entry is re-exported, so a Node app
// can import from here alone.
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

import { A12sClient } from "./index.js";

export * from "./index.js";

/** Connects as the owner of a running Fleet, using the ~/.a12s/<fleet>/controller.json that `a12s up` writes. */
export async function fromFleet(fleet: string, opts: { home?: string; fetch?: typeof fetch } = {}): Promise<A12sClient> {
  const path = join(opts.home ?? homedir(), ".a12s", fleet, "controller.json");
  let cf: { port: number; token: string };
  try {
    cf = JSON.parse(await readFile(path, "utf8"));
  } catch (err) {
    throw new Error(`no running Controller for Fleet ${fleet} (${path}): run a12s up first`, { cause: err });
  }
  return new A12sClient({ url: `http://127.0.0.1:${cf.port}`, token: cf.token, fetch: opts.fetch });
}
