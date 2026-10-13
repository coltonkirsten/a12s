import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();
const $ = (id) => document.getElementById(id);
const log = $("log");
const open = new Map(); // computer/turn -> the element its text streams into

function line(computer, html, cls = "") {
  const div = document.createElement("div");
  div.className = `ev ${cls}`;
  div.innerHTML = `<span class="who">${esc(computer)}</span> ${html}`;
  const follow = log.scrollTop + log.clientHeight >= log.scrollHeight - 4;
  log.append(div);
  if (follow) log.scrollTop = log.scrollHeight;
  return div;
}
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

async function refresh() {
  const st = await fleet.status();
  $("fleet").textContent = st.fleet;
  $("computers").innerHTML = st.agentComputers.map((c) => `
    <div class="computer"><b>${esc(c.name)}</b><span class="phase">${esc(c.phase)}</span>
    <div class="meta">${esc(c.harness.kind)} ${esc(c.harness.model ?? "")} · inbox ${c.inbox?.pending ?? 0}</div></div>`).join("");
  const to = $("to");
  if (to.options.length !== st.agentComputers.length) {
    to.innerHTML = st.agentComputers.map((c) => `<option>${esc(c.name)}</option>`).join("");
  }
}
await refresh();
setInterval(() => refresh().catch(() => {}), 3000);

$("send").addEventListener("submit", async (e) => {
  e.preventDefault();
  const body = $("body").value.trim();
  if (!body) return;
  $("body").value = "";
  try {
    await (await fleet.agentComputers.get($("to").value)).send(body, { sender: "dashboard" });
  } catch (err) {
    line($("to").value, esc(err.message), "err");
  }
});

for await (const ev of fleet.events()) {
  const key = `${ev.agentComputer}/${ev.turn}`;
  switch (ev.type) {
    case "turn.started": {
      const from = (ev.data.inbox?.messages ?? []).map((m) => m.via).join(", ");
      line(ev.agentComputer, `<span class="dim">turn started${from ? ` · from ${esc(from)}` : ""}</span>`);
      break;
    }
    case "text.delta": {
      let el = open.get(key);
      if (!el) open.set(key, (el = line(ev.agentComputer, "").appendChild(document.createElement("span"))));
      el.textContent += ev.data.text;
      break;
    }
    case "tool.started":
      open.delete(key);
      line(ev.agentComputer, `<span class="dim">→ ${esc(ev.data.name)}</span>`);
      break;
    case "turn.finished":
      open.delete(key);
      line(ev.agentComputer, `<span class="${ev.data.status === "completed" ? "dim" : "err"}">turn ${esc(ev.data.status)}${ev.data.error ? `: ${esc(ev.data.error)}` : ""}</span>`);
      break;
    case "resume.failed":
    case "gap":
      line(ev.agentComputer, `<span class="err">${esc(ev.type)} ${esc(JSON.stringify(ev.data))}</span>`);
      break;
  }
}
