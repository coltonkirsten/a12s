"""The object SDK against a fake Controller: the routes, headers and bodies each call sends."""

from __future__ import annotations

import dataclasses
import itertools
import json
import threading
from typing import Any

import httpx
import pytest

import a12s

STATUS = {"name": "hello", "phase": "Ready", "harness": {"kind": "claude-code", "version": "2.1.280"}, "image": "slim"}
HELLO = {"name": "hello", "spec": {"instructions": "Be helpful"}, "effective": {"image": "slim"}, "status": STATUS}


class Fake:
    def __init__(self) -> None:
        self.seen: list[tuple[str, str, dict[str, str], Any]] = []
        self.version = 7
        self.conflicts = 0
        self.held = False

    def handler(self, req: httpx.Request) -> httpx.Response:
        body = json.loads(req.content) if req.content else None
        path = req.url.raw_path.decode()
        self.seen.append((req.method, path, dict(req.headers), body))
        if req.method == "PUT" and path == "/v1/agent-computers/hello":
            if req.headers.get("if-none-match") == "*" and self.version > 7:
                return httpx.Response(409, json={"code": "exists", "message": "agent computer hello already exists"})
            self.version += 1
            return httpx.Response(200, json={"version": self.version, "changes": [], "agentComputer": HELLO})
        if req.method == "PATCH" and path == "/v1/agent-computers/hello":
            spec = {**HELLO["spec"], **body}
            return httpx.Response(200, json={"version": self.version, "agentComputer": {**HELLO, "spec": spec}})
        if req.method == "GET" and path == "/v1/agent-computers/nope":
            return httpx.Response(404, json={"code": "not_found", "message": 'no agent computer "nope" in the Fleet'})
        if req.method == "POST" and path == "/v1/agent-computers/hello/inbox":
            return httpx.Response(201, json={"id": "m_1", "seq": 1, "createdAt": "t", "expiresAt": None, "state": "pending", "position": 1})
        if req.method == "GET" and path == "/v1/agent-computers/hello/inbox/m_1/events":
            snap = {"message": {"id": "m_1", "state": "pending" if self.held else "delivered", "turn": "t_1"}, "held": self.held}
            return sse({"type": "snapshot", "data": snap})
        if req.method == "GET" and path == "/v1/agent-computers/hello/events?turn=t_1":
            ev = {"v": 1, "computer": "hello", "session": "s", "turn": "t_1", "ts": "t"}
            return sse(
                {**ev, "seq": 1, "type": "text.delta", "data": {"text": "out of credit"}},
                {**ev, "seq": 2, "type": "turn.finished", "data": {"status": "failed", "error": "billing", "usage": None}},
            )
        if req.method == "DELETE" and path == "/v1/agent-computers/hello":
            return httpx.Response(200, json={"version": self.version, "removed": ["hello"]})
        if req.method == "GET" and path == "/v1/fleet":
            return httpx.Response(200, json={"version": self.version, "fleet": {"agent_computers": {}}})
        if req.method == "PUT" and path == "/v1/fleet":
            if self.conflicts:
                self.conflicts -= 1
                self.version += 1
                return httpx.Response(412, json={"code": "version_mismatch", "message": "stale"})
            return httpx.Response(200, json={"version": self.version + 1})
        return httpx.Response(500, json={"code": "unexpected", "message": f"{req.method} {path}"})


def sse(*events: dict[str, Any]) -> httpx.Response:
    body = "".join(f"data: {json.dumps(e)}\n\n" for e in events)
    return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())


def fleet_for(fake: Fake) -> a12s.Fleet:
    return a12s.Fleet("http://x", "tok", http=httpx.Client(transport=httpx.MockTransport(fake.handler)))


def test_create_update_send_remove() -> None:
    fake = Fake()
    fleet = fleet_for(fake)
    hello = fleet.agent_computers.create("hello", instructions="Be helpful")
    method, path, headers, body = fake.seen[-1]
    assert (method, path, headers["if-none-match"], body) == ("PUT", "/v1/agent-computers/hello", "*", {"instructions": "Be helpful"})
    assert (hello.name, hello.status.phase, hello.spec) == ("hello", "Ready", {"instructions": "Be helpful"})

    with pytest.raises(a12s.A12sError) as e:
        fleet.agent_computers.create("hello", instructions="again")
    assert e.value.code == "exists"
    fleet.agent_computers.create("hello", replace=True, instructions="again")
    assert "if-none-match" not in fake.seen[-1][2]

    hello.update(egress={"allow": ["github.com"]})
    assert fake.seen[-1][0::3] == ("PATCH", {"egress": {"allow": ["github.com"]}})
    assert hello.spec["egress"] == {"allow": ["github.com"]}

    assert hello.send("hi").id == "m_1"
    assert fake.seen[-1][1] == "/v1/agent-computers/hello/inbox"
    hello.remove()
    assert fake.seen[-1][:2] == ("DELETE", "/v1/agent-computers/hello")


def test_wait_after_the_turn_ended_returns_it_failed_turns_included() -> None:
    fake = Fake()
    hello = fleet_for(fake).agent_computers.create("hello")
    m = hello.send("hi", fresh=True)
    assert isinstance(m, a12s.EnqueueResponse) and (m.id, m.state) == ("m_1", "pending")
    assert dataclasses.asdict(m)["id"] == "m_1"
    seen: list[str] = []
    r = m.wait(on_event=lambda e: seen.append(e.type))
    assert (r.id, r.status, r.text, r.message.id) == ("t_1", "failed", "out of credit", "m_1")
    assert seen == ["text.delta", "turn.finished"]
    assert hello.send("again").wait().id == "t_1"
    fake.held = True
    with pytest.raises(a12s.A12sError) as e:
        hello.send("held").wait()
    assert e.value.code == "held"


def test_get_missing_raises_not_found() -> None:
    with pytest.raises(a12s.NotFound) as e:
        fleet_for(Fake()).agent_computers.get("nope")
    assert isinstance(e.value, a12s.A12sError) and e.value.status == 404


def test_replace_retries_a_conflict_with_if_match() -> None:
    fake = Fake()
    fake.conflicts = 1
    state = fleet_for(fake).replace(lambda f: f.defaults.update(power={"sleepAfterIdle": "15m"}))
    puts = [s for s in fake.seen if s[0] == "PUT"]
    assert [p[2]["if-match"] for p in puts] == ['"7"', '"8"']
    assert puts[-1][3]["fleet"]["defaults"] == {"power": {"sleepAfterIdle": "15m"}}
    assert state.version == fake.version


@pytest.mark.anyio
async def test_async_fleet_has_the_same_shape() -> None:
    fake = Fake()
    fleet = a12s.AsyncFleet("http://x", "tok", http=httpx.AsyncClient(transport=httpx.MockTransport(fake.handler)))
    hello = await fleet.agent_computers.create("hello", instructions="Be helpful")
    assert fake.seen[-1][2]["if-none-match"] == "*"
    assert (await hello.send("hi")).id == "m_1"
    assert (await hello.send("hi").wait()).status == "failed"
    m = await hello.send("hi")
    assert (await m.wait()).message.id == "m_1"
    await hello.remove()
    assert fake.seen[-1][:2] == ("DELETE", "/v1/agent-computers/hello")
    await fleet.aclose()


def fleet_event(n: int, name: str, typ: str, turn: str = "t_1", data: dict[str, Any] | None = None) -> str:
    ev = {"v": 1, "agentComputer": name, "computer": name, "session": "s", "turn": turn, "seq": n, "ts": "t", "type": typ}
    return f"id: b-{n}\ndata: {json.dumps({**ev, 'data': data or {}})}\n\n"


def test_events_share_one_connection_and_filter(fake: Any) -> None:
    opened: list[str | None] = []
    sent = threading.Event()

    def handler(req: Any, res: Any) -> None:
        if req.url == "/v1/agent-computers/a/turns?limit=2":
            turn = {"agentComputer": "a", "id": "t_0", "session": "s", "status": "completed", "text": "old",
                    "messages": [], "tools": [], "usage": None, "tokens": {"input": 1, "output": 2, "cacheRead": 0, "cacheWrite": 0},
                    "finishedAt": "t"}
            res.json(200, {"turns": [turn]})
            return
        assert req.url == "/v1/events"
        opened.append(req.headers.get("last-event-id"))
        res.write_head(200)
        res.write('event: head\ndata: {"id":"b-0"}\n\n')
        sent.wait(5)
        res.write(fleet_event(1, "a", "turn.started"))
        res.write(fleet_event(2, "b", "turn.started", "t_2"))
        res.write(fleet_event(3, "a", "text.delta", data={"text": "hi"}))
        res.write(fleet_event(4, "a", "turn.finished", data={"status": "completed", "tokens": {"input": 3, "output": 1, "cacheRead": 0, "cacheWrite": 0}}))

    server = fake(handler)
    fleet = a12s.Fleet(server.url, "tok")
    a = a12s.AgentComputer(fleet, {"name": "a", "status": {"name": "a"}})
    with a.events() as mine, fleet.events() as everything, a.turns() as turns:
        sent.set()
        assert [e.seq for e in itertools.islice(mine, 3)] == [1, 3, 4]
        assert [e.agent_computer for e in itertools.islice(everything, 4)] == ["a", "b", "a", "a"]
        t = next(turns)
        assert (t.agent_computer, t.text, t.tokens) == ("a", "hi", a12s.TokenUsage(input=3, output=1, cache_read=0, cache_write=0))
    assert opened == [None], "one connection for all three streams"
    past = a.turns(limit=2)
    assert [(p.agent_computer, p.text, p.tokens and p.tokens.output) for p in past] == [("a", "old", 2)]
    fleet.close()
