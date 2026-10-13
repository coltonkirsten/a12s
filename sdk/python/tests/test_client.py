from __future__ import annotations

import asyncio
import itertools
import json
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest
from conftest import FakeServer, Handler, Req, Res, ev

import a12s
from a12s import A12sError, A12sGapError, A12sStreamError
from a12s._connect import _fleet_name

Fake = Callable[[Handler], FakeServer]


def collect(stream: Any, n: int) -> list[Any]:
    try:
        return list(itertools.islice(stream, n))
    finally:
        stream.close()


async def acollect(stream: Any, n: int) -> list[Any]:
    out: list[Any] = []
    try:
        async for x in stream:
            out.append(x)
            if len(out) == n:
                break
    finally:
        await stream.aclose()
    return out


def prompt_server() -> tuple[Handler, dict[str, int]]:
    calls = {"events": 0}

    def handler(req: Req, res: Res) -> None:
        assert req.headers["authorization"] == "Bearer tok"
        if req.method == "POST" and req.url == "/v1/agent-computers/lead/inbox":
            m = json.loads(req.body)
            assert m["body"] == "split this"
            assert m["idempotencyKey"], "the client always sends an idempotency key"
            res.json(201, {"id": "m_1", "seq": 1, "state": "pending"})
            return
        res.write_head(200)
        if req.url == "/v1/agent-computers/lead/inbox/m_1/events":
            res.write(ev(1, "snapshot", "", {"message": {"id": "m_1", "state": "pending"}, "held": False}))
            res.write(": keepalive\n\n")
            res.end(ev(2, "delivered", "", {"turn": "t_9"}))
            return
        assert req.url == "/v1/agent-computers/lead/events?turn=t_9"
        calls["events"] += 1
        if calls["events"] == 1:
            res.write('event: head\ndata: {"seq":0}\n\n')
            res.end(ev(10, "text.delta", "t_9", {"text": "hel"}))
            return
        assert req.headers["last-event-id"] == "10", "reconnect resumes after the last seq"
        res.write(ev(11, "text.delta", "t_9", {"text": "lo"}))
        res.end(ev(12, "turn.finished", "t_9", {"status": "completed", "usage": {}}))

    return handler, calls


def test_prompt_streams_the_turn_across_a_reconnect(fake: Fake) -> None:
    handler, _ = prompt_server()
    seen: list[str] = []
    with fake(handler).client() as client:
        r = client.prompt("lead", "split this", on_event=lambda e: seen.append(e.type))
    assert [r.id, r.status, r.text, r.message.id] == ["t_9", "completed", "hello", "m_1"]
    assert seen == ["text.delta", "text.delta", "turn.finished"]


@pytest.mark.anyio
async def test_async_prompt_streams_the_turn_across_a_reconnect(fake: Fake) -> None:
    handler, _ = prompt_server()
    seen: list[str] = []
    async with fake(handler).aclient() as client:
        r = await client.prompt("lead", "split this", on_event=lambda e: seen.append(e.type))
    assert [r.id, r.status, r.text, r.message.id] == ["t_9", "completed", "hello", "m_1"]
    assert seen == ["text.delta", "text.delta", "turn.finished"]


def refuse(_: httpx.Request) -> httpx.Response:
    body = {"code": "no_grant", "message": "worker1 may not send to lead", "fix": "add lead to grants.send"}
    return httpx.Response(403, json=body)


def assert_no_grant(e: A12sError) -> None:
    assert e.status == 403
    assert e.code == "no_grant"
    assert "fix: add lead to grants.send" in str(e)


def test_errors_carry_the_code_and_fix_line() -> None:
    client = a12s.Client("http://x", "t", http=httpx.Client(transport=httpx.MockTransport(refuse)))
    with pytest.raises(A12sError) as e:
        client.send("lead", "x")
    assert_no_grant(e.value)


@pytest.mark.anyio
async def test_async_errors_carry_the_code_and_fix_line() -> None:
    client = a12s.AsyncClient("http://x", "t", http=httpx.AsyncClient(transport=httpx.MockTransport(refuse)))
    with pytest.raises(A12sError) as e:
        await client.send("lead", "x")
    assert_no_grant(e.value)


def gap_server(req: Req, res: Res) -> None:
    assert req.headers["last-event-id"] == "41"
    res.write_head(200)
    res.write('event: gap\ndata: {"lastEventId":"41","head":7}\n\n')
    res.write(ev(8, "diagnostic", "", {"message": "after gap"}))


def assert_gap(gap: a12s.Event, nxt: a12s.Event) -> None:
    assert gap.type == "gap"
    assert gap.computer == "lead"
    assert gap.data == {"lastEventId": "41", "head": 7, "after": 41}
    assert nxt.seq == 8


def test_events_yield_a_gap_then_go_on_live(fake: Fake) -> None:
    gap, nxt = collect(fake(gap_server).client().events("lead", after=41), 2)
    assert_gap(gap, nxt)


@pytest.mark.anyio
async def test_async_events_yield_a_gap_then_go_on_live(fake: Fake) -> None:
    async with fake(gap_server).aclient() as client:
        gap, nxt = await acollect(client.events("lead", after=41), 2)
    assert_gap(gap, nxt)


def test_events_on_gap_throw(fake: Fake) -> None:
    with pytest.raises(A12sGapError) as e:
        collect(fake(gap_server).client().events("lead", after=41, on_gap="throw"), 1)
    assert e.value.after == 41


@pytest.mark.anyio
async def test_async_events_on_gap_throw(fake: Fake) -> None:
    with pytest.raises(A12sGapError) as e:
        async with fake(gap_server).aclient() as client:
            await acollect(client.events("lead", after=41, on_gap="throw"), 1)
    assert e.value.after == 41


def malformed_server() -> tuple[Handler, dict[str, int]]:
    calls = {"n": 0}

    def handler(_: Req, res: Res) -> None:
        calls["n"] += 1
        res.write_head(200)
        res.write(ev(1, "diagnostic", "", {}))
        res.write("id: 2\ndata: {not json\n\n")

    return handler, calls


def test_events_malformed_event_raises_instead_of_reconnecting(fake: Fake) -> None:
    handler, calls = malformed_server()
    with pytest.raises(A12sStreamError, match="malformed event after seq 1"):
        collect(fake(handler).client().events("lead", reconnect=0.01), 5)
    assert calls["n"] == 1


@pytest.mark.anyio
async def test_async_events_malformed_event_raises_instead_of_reconnecting(fake: Fake) -> None:
    handler, calls = malformed_server()
    with pytest.raises(A12sStreamError, match="malformed event after seq 1"):
        async with fake(handler).aclient() as client:
            await acollect(client.events("lead", reconnect=0.01), 5)
    assert calls["n"] == 1


def dropping_server() -> tuple[Handler, dict[str, int]]:
    calls = {"n": 0}

    def handler(_: Req, res: Res) -> None:
        calls["n"] += 1
        res.write_head(200)
        res.write(ev(calls["n"], "diagnostic", "", {}))
        if calls["n"] == 1:
            time.sleep(0.02)
            res.destroy()  # a network drop mid-stream

    return handler, calls


def test_events_reconnect_after_a_drop_and_stop_after_a_consumer_error(fake: Fake) -> None:
    handler, calls = dropping_server()
    stream = fake(handler).client().events("lead", reconnect=0.01)
    assert next(stream).seq == 1
    assert next(stream).seq == 2, "reconnected after the drop"
    assert calls["n"] == 2
    with pytest.raises(RuntimeError, match="consumer bug"):
        try:
            raise RuntimeError("consumer bug")
        finally:
            stream.close()
    assert list(stream) == []
    time.sleep(0.1)
    assert calls["n"] == 2, "no reconnect after a consumer error"


@pytest.mark.anyio
async def test_async_events_reconnect_after_a_drop_and_stop_after_a_consumer_error(fake: Fake) -> None:
    handler, calls = dropping_server()
    client = fake(handler).aclient()
    stream = client.events("lead", reconnect=0.01)
    assert (await stream.__anext__()).seq == 1
    assert (await stream.__anext__()).seq == 2, "reconnected after the drop"
    assert calls["n"] == 2
    with pytest.raises(RuntimeError, match="consumer bug"):
        try:
            raise RuntimeError("consumer bug")
        finally:
            await stream.aclose()
    assert [e async for e in stream] == []
    await asyncio.sleep(0.1)
    assert calls["n"] == 2, "no reconnect after a consumer error"
    await client.aclose()


def broken_transport(_: httpx.Request) -> httpx.Response:
    raise ValueError("bad transport")


def test_events_non_network_failure_is_raised() -> None:
    client = a12s.Client("http://x", "t", http=httpx.Client(transport=httpx.MockTransport(broken_transport)))
    with pytest.raises(ValueError, match="bad transport"):
        collect(client.events("lead"), 1)


@pytest.mark.anyio
async def test_async_events_non_network_failure_is_raised() -> None:
    http = httpx.AsyncClient(transport=httpx.MockTransport(broken_transport))
    with pytest.raises(ValueError, match="bad transport"):
        await acollect(a12s.AsyncClient("http://x", "t", http=http).events("lead"), 1)


@pytest.fixture
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for k in ("A12S_URL", "A12S_TOKEN", "A12S_FLEET"):
        monkeypatch.delenv(k, raising=False)


def write_controller(home: Path, fleet: str, port: int) -> None:
    (home / ".a12s" / fleet).mkdir(parents=True, exist_ok=True)
    (home / ".a12s" / fleet / "controller.json").write_text(json.dumps({"port": port, "token": "tk"}))


@pytest.mark.usefixtures("clean_env")
def test_connect_discovery(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    home, nowhere = tmp_path / "home", tmp_path / "cwd"
    home.mkdir()
    nowhere.mkdir()
    with pytest.raises(RuntimeError, match="no running Fleet found"):
        a12s.connect(home=home, cwd=nowhere)
    write_controller(home, "proj", 52011)
    assert a12s.connect(home=home, cwd=nowhere).url == "http://127.0.0.1:52011", "the only running Fleet"
    write_controller(home, "other", 52012)
    with pytest.raises(RuntimeError, match=r"several Fleets are running \(other, proj\)"):
        a12s.connect(home=home, cwd=nowhere)
    assert a12s.connect(home=home, fleet="other").url == "http://127.0.0.1:52012"

    project = nowhere / "app" / "src"
    project.mkdir(parents=True)
    (nowhere / "fleet.yaml").write_text("apiVersion: a12s.ai/v1alpha1\nkind: Fleet\nmetadata: { name: proj }\n")
    assert a12s.connect(home=home, cwd=project).url == "http://127.0.0.1:52011", "nearest fleet.yaml upward"
    with pytest.raises(RuntimeError, match="Fleet nope isn't running"):
        a12s.connect(home=home, fleet="nope")
    assert a12s.connect(url="http://h:1/", token="x").url == "http://h:1"

    (tmp_path / "other.yaml").write_text("metadata:\n  name: other\n")
    monkeypatch.setenv("A12S_FLEET", "../other.yaml")
    assert a12s.connect(home=home, cwd=nowhere).url == "http://127.0.0.1:52012", "$A12S_FLEET"
    monkeypatch.setenv("A12S_URL", "http://env:2")
    monkeypatch.setenv("A12S_TOKEN", "envtok")
    assert a12s.connect(home=home).url == "http://env:2"


@pytest.mark.anyio
@pytest.mark.usefixtures("clean_env")
async def test_connect_async_discovery(tmp_path: Path) -> None:
    with pytest.raises(RuntimeError, match="no running Fleet found"):
        await a12s.connect_async(home=tmp_path, cwd=tmp_path)
    write_controller(tmp_path, "proj", 52011)
    client = await a12s.connect_async(home=tmp_path, cwd=tmp_path)
    assert isinstance(client, a12s.AsyncClient)
    assert client.url == "http://127.0.0.1:52011"


def test_fleet_name_reads_flow_and_block_metadata() -> None:
    assert _fleet_name("metadata: { name: starter }\n") == "starter"
    assert _fleet_name('kind: Fleet\nmetadata:\n  labels: {}\n  name: "lab"\n') == "lab"
    assert _fleet_name("kind: Fleet\n") is None


def merged_server(req: Req, res: Res) -> None:
    if req.url == "/v1/status":
        res.json(200, {"fleet": "f", "agentComputers": [{"name": "lead"}, {"name": "w1"}, {"name": "w2"}]})
        return
    if req.url == "/v1/agent-computers/w2/events":
        res.json(503, {"code": "asleep", "message": "asleep"})
        return
    name = req.url.split("/")[3]
    res.write_head(200)
    res.write('event: head\ndata: {"seq":5}\n\n')
    time.sleep(0.03)
    started = {"inbox": {"batch": "b", "pendingAfter": 0, "messages": [{"id": "m", "via": "local", "sender": "app"}]}}
    res.write(ev(6, "turn.started", f"t_{name}", started, computer=name))
    res.write(ev(7, "text.delta", f"t_{name}", {"text": f"hi from {name}"}, computer=name))
    res.write(ev(8, "tool.started", f"t_{name}", {"id": "u", "name": "mcp__a12s__send"}, computer=name))
    res.write(ev(9, "text.delta", f"t_{name}", {"text": "SENT"}, computer=name))
    res.write(ev(10, "turn.finished", f"t_{name}", {"status": "completed", "usage": None}, computer=name))


def test_events_merge_every_computer_and_turns_fold_them(fake: Fake) -> None:
    got: list[str] = []
    with fake(merged_server).client().turns(reconnect=0.05) as turns:
        for t in turns:
            assert t.messages[0].via == "local"
            assert [x.name for x in t.tools] == ["mcp__a12s__send"]
            got.append(f"{t.computer}:{t.status}:{t.text}")
            if len(got) == 2:
                break
    assert sorted(got) == ["lead:completed:hi from lead\nSENT", "w1:completed:hi from w1\nSENT"]


@pytest.mark.anyio
async def test_async_events_merge_every_computer_and_turns_fold_them(fake: Fake) -> None:
    got: list[str] = []
    async with fake(merged_server).aclient() as client, client.turns(reconnect=0.05) as turns:
        async for t in turns:
            assert t.messages[0].via == "local"
            assert [x.name for x in t.tools] == ["mcp__a12s__send"]
            got.append(f"{t.computer}:{t.status}:{t.text}")
            if len(got) == 2:
                break
    assert sorted(got) == ["lead:completed:hi from lead\nSENT", "w1:completed:hi from w1\nSENT"]


def head_server() -> Handler:
    calls = {"n": 0}

    def handler(req: Req, res: Res) -> None:
        calls["n"] += 1
        res.write_head(200)
        if calls["n"] == 1:
            res.write('event: head\ndata: {"seq":41}\n\n')
            time.sleep(0.02)
            res.destroy()
            return
        assert req.headers["last-event-id"] == "41"
        res.write(ev(42, "diagnostic", "", {"message": "after the drop"}))

    return handler


def test_events_reconnect_resumes_from_the_head(fake: Fake) -> None:
    (e,) = collect(fake(head_server()).client().events("lead", reconnect=0.01), 1)
    assert e.seq == 42


@pytest.mark.anyio
async def test_async_events_reconnect_resumes_from_the_head(fake: Fake) -> None:
    async with fake(head_server()).aclient() as client:
        (e,) = await acollect(client.events("lead", reconnect=0.01), 1)
    assert e.seq == 42


def not_found(_: Req, res: Res) -> None:
    res.json(404, {"code": "not_found", "message": "no Computer x"})


def test_events_ready_raises_when_a_computer_does_not_exist(fake: Fake) -> None:
    stream = fake(not_found).client().events("x")
    with pytest.raises(A12sError) as e:
        stream.ready()
    assert e.value.code == "not_found"
    with pytest.raises(A12sError), stream:
        pass


@pytest.mark.anyio
async def test_async_events_ready_raises_when_a_computer_does_not_exist(fake: Fake) -> None:
    async with fake(not_found).aclient() as client:
        stream = client.events("x")
        with pytest.raises(A12sError) as e:
            await stream.ready()
        assert e.value.code == "not_found"
        with pytest.raises(A12sError):
            async with stream:
                pass


def test_events_turn_and_after_need_one_computer() -> None:
    with pytest.raises(TypeError):
        a12s.Client("http://x").events(["a", "b"], after=1)
    with pytest.raises(TypeError):
        a12s.AsyncClient("http://x").events(None, turn="t")


def test_stop_defaults_to_the_active_turn() -> None:
    seen: list[str] = []

    def handler(req: httpx.Request) -> httpx.Response:
        seen.append(f"{req.method} {req.url.path}")
        if req.url.path == "/v1/status":
            return httpx.Response(200, json={"agentComputers": [{"name": "lead", "activeTurn": "t_1"}, {"name": "idle"}]})
        assert req.content == b"{}"
        return httpx.Response(204)

    client = a12s.Client("http://x", http=httpx.Client(transport=httpx.MockTransport(handler)))
    client.stop("lead")
    client.stop("idle")
    with pytest.raises(A12sError, match="not_found"):
        client.stop("nope")
    assert seen == ["GET /v1/status", "POST /v1/agent-computers/lead/turns/t_1/stop", "GET /v1/status", "GET /v1/status"]

