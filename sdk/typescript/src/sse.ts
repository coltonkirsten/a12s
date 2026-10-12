export interface SSEFrame {
  id?: string;
  event?: string;
  data: string;
}

/** Parses a text/event-stream body into frames. Comments (keepalives) are skipped. */
export async function* readSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEFrame> {
  const decoder = new TextDecoder();
  let buf = "";
  let frame: { id?: string; event?: string; data: string[] } = { data: [] };
  const reader = body.getReader();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.search(/\r\n|\r|\n/)) >= 0) {
        if (buf[nl] === "\r" && nl === buf.length - 1) break; // a CRLF split across chunks
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + (buf.startsWith("\r\n", nl) ? 2 : 1));
        if (line === "") {
          if (frame.data.length > 0 || frame.event) {
            yield { id: frame.id, event: frame.event, data: frame.data.join("\n") };
          }
          frame = { data: [] };
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let val = colon < 0 ? "" : line.slice(colon + 1);
        if (val.startsWith(" ")) val = val.slice(1);
        if (field === "data") frame.data.push(val);
        else if (field === "id") frame.id = val;
        else if (field === "event") frame.event = val;
      }
    }
  } finally {
    reader.releaseLock();
  }
}
