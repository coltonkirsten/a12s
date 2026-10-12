// The default entry (browsers, Deno, workers): no node: imports. Node resolves "a12s" to ./node.ts instead, through
// the "node" export condition, so `import { connect } from "a12s"` works everywhere.
import { A12sClient, type A12sClientOptions } from "./client.js";

export * from "./client.js";
export * from "./types.js";

export type ConnectOptions = A12sClientOptions;

/**
 * In a browser: a client for the page's own origin, which is `a12s gateway` serving the page (no token needed).
 * Pass `url` and `token` to reach anything else.
 */
export async function connect(opts: ConnectOptions = {}): Promise<A12sClient> {
  return new A12sClient(opts);
}
