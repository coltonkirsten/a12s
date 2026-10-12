"""The fixtures are rendered from the Go API types (internal/api/sdkfixtures_test.go). Parsing them strictly fails when
Go adds, drops or retypes a field the Python types don't know about."""

from __future__ import annotations

import dataclasses
import json
import re
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest

import a12s
from a12s import (
    EnqueueResponse,
    Event,
    InboxList,
    Message,
    ServiceStatus,
    Status,
    Token,
    TurnInboxMessage,
)
from a12s._core import TurnBuilder
from a12s._parse import _fields, from_json

FIXTURES: dict[str, Any] = json.loads((Path(__file__).parents[2] / "fixtures" / "api.json").read_text())

# fixture name -> the dataclass it parses into, and where in the fixture that object is (None: absent there)
PARSED: dict[str, tuple[type, Callable[[Any], Any]]] = {
    "statusResponse": (Status, lambda f: f),
    "enqueueResponse": (EnqueueResponse, lambda f: f),
    "message": (Message, lambda f: f),
    "inboxListResponse": (InboxList, lambda f: f),
    "servicesResponse": (ServiceStatus, lambda f: f["services"][0]),
    "token": (Token, lambda f: f),
    "event": (Event, lambda f: f),
    "turnStartedData": (TurnInboxMessage, lambda f: f["inbox"]["messages"][0] if "inbox" in f else None),
}
# Requests the client builds from keyword arguments, and responses it passes through as dicts.
REQUESTS = {"enqueueRequest", "tokenRequest"}
RAW = {"putFleetResponse", "error"}
# Event data stays the raw JSON dict; the ones TurnBuilder reads are folded below.
EVENT_DATA = {"diagnosticData", "resumeFailedData", "textDeltaData", "turnFinishedData"}


def snake(key: str) -> str:
    return re.sub(r"[A-Z]", lambda m: "_" + m.group().lower(), key)


def variants(name: str) -> list[tuple[str, Any]]:
    return [(name, FIXTURES[name]), (name + "Minimal", FIXTURES[name + "Minimal"])]


def test_every_fixture_is_covered() -> None:
    names = {k.removesuffix("Minimal") for k in FIXTURES}
    assert names == set(PARSED) | REQUESTS | RAW | EVENT_DATA, "a new API type: model it in a12s/_types.py"


@pytest.mark.parametrize("name", sorted(PARSED))
def test_fixture_parses_strictly(name: str) -> None:
    cls, pick = PARSED[name]
    for label, fx in variants(name):
        obj = pick(fx)
        if obj is None:
            continue
        from_json(cls, obj, strict=True)
        if label.endswith("Minimal"):
            assert_required_keys(cls, obj, label)


def assert_required_keys(cls: type, data: Any, path: str) -> None:
    """A field Go always sends is required in Python, and a field it may omit is optional."""
    if isinstance(data, list):
        for i, x in enumerate(data):
            assert_required_keys(cls, x, f"{path}[{i}]")
        return
    if not (dataclasses.is_dataclass(cls) and isinstance(data, dict)):
        return
    fields = _fields(cls)
    assert {f.key for f in fields if f.required} == set(data), path
    for f in fields:
        if f.key in data:
            assert_required_keys(_inner(f.hint), data[f.key], f"{path}.{f.key}")


def _inner(hint: Any) -> Any:
    args = [a for a in getattr(hint, "__args__", ()) if a is not type(None)]
    return _inner(args[-1]) if args else hint


def test_strict_mode_rejects_unknown_and_missing_keys() -> None:
    tok = dict(FIXTURES["token"])
    with pytest.raises(ValueError, match="unknown keys"):
        from_json(Token, {**tok, "newField": 1}, strict=True)
    with pytest.raises(ValueError, match="missing createdAt"):
        from_json(Token, {k: v for k, v in tok.items() if k != "createdAt"}, strict=True)
    with pytest.raises(ValueError, match="want int"):
        from_json(EnqueueResponse, {**FIXTURES["enqueueResponse"], "seq": "1"}, strict=True)
    assert from_json(Token, {**tok, "newField": 1}).created_at == tok["createdAt"], "forward compatible"


def test_turn_builder_folds_the_turn_event_data() -> None:
    for suffix in ("", "Minimal"):
        started, delta, finished = (
            FIXTURES[n + suffix] for n in ("turnStartedData", "textDeltaData", "turnFinishedData")
        )

        def e(type: str, data: Any, ts: str) -> Event:
            return Event(v=1, computer="c", session="s", turn="t", seq=1, ts=ts, type=type, data=data)

        b = TurnBuilder()
        assert b.add(e("turn.started", started, "t0")) is None
        assert b.add(e("text.delta", delta, "t1")) is None
        turn = b.add(e("turn.finished", finished, "t2"))
        assert turn is not None
        assert (turn.status, turn.text, turn.usage, turn.error) == ("completed", "text", {}, finished.get("error"))
        assert (turn.started_at, turn.finished_at) == ("t0", "t2")
        assert [m.id for m in turn.messages] == [m["id"] for m in started.get("inbox", {}).get("messages", [])]


def recorder(routes: dict[str, Any]) -> tuple[httpx.MockTransport, list[Any]]:
    sent: list[Any] = []

    def handler(req: httpx.Request) -> httpx.Response:
        sent.append(json.loads(req.content) if req.content else None)
        body = routes.get(f"{req.method} {req.url.raw_path.decode()}")
        if body is None:
            return httpx.Response(403, json=FIXTURES["error"])
        return httpx.Response(200, json=body)

    return httpx.MockTransport(handler), sent


ROUTES = {
    "GET /v1/status": FIXTURES["statusResponse"],
    "POST /v1/computers/lead/inbox": FIXTURES["enqueueResponse"],
    "GET /v1/computers/lead/inbox": FIXTURES["inboxListResponse"],
    "GET /v1/computers/lead/inbox/m_1": FIXTURES["message"],
    "GET /v1/services": FIXTURES["servicesResponse"],
    "GET /v1/services/svc": FIXTURES["servicesResponse"]["services"][0],
    "POST /v1/tokens": FIXTURES["token"],
    "PUT /v1/fleet": FIXTURES["putFleetResponse"],
}


def test_client_parses_every_fixture_off_the_wire() -> None:
    transport, sent = recorder(ROUTES)
    client = a12s.Client("http://x", "t", http=httpx.Client(transport=transport))
    assert client.status() == from_json(Status, FIXTURES["statusResponse"], strict=True)
    assert client.inbox("lead") == from_json(InboxList, FIXTURES["inboxListResponse"], strict=True)
    assert client.message("lead", "m_1") == from_json(Message, FIXTURES["message"], strict=True)
    services = [from_json(ServiceStatus, s, strict=True) for s in FIXTURES["servicesResponse"]["services"]]
    assert client.services() == services
    assert client.service("svc") == services[0]
    assert client.put_fleet({"fleet": {"metadata": {"name": "f"}}}) == FIXTURES["putFleetResponse"]
    with pytest.raises(a12s.A12sError) as e:
        client.withdraw("lead", "m_1")
    err = FIXTURES["error"]
    assert set(err) == {"code", "message", "fix", "details"}
    assert (e.value.code, e.value.message, e.value.fix, e.value.details) == (
        err["code"],
        err["message"],
        err["fix"],
        err["details"],
    )
    assert str(e.value) == "code: message  fix: fix"


def test_send_and_create_token_send_exactly_the_request_keys() -> None:
    transport, sent = recorder(ROUTES)
    client = a12s.Client("http://x", "t", http=httpx.Client(transport=transport))

    req = FIXTURES["enqueueRequest"]
    kwargs = {snake(k): v for k, v in req.items() if k != "body"}
    assert client.send("lead", req["body"], **kwargs) == from_json(EnqueueResponse, FIXTURES["enqueueResponse"])
    assert sent[-1] == req
    client.send("lead", FIXTURES["enqueueRequestMinimal"]["body"])
    assert set(sent[-1]) == set(FIXTURES["enqueueRequestMinimal"]) | {"idempotencyKey"}

    for name in ("tokenRequest", "tokenRequestMinimal"):
        req = FIXTURES[name]
        assert client.create_token(**{snake(k): v for k, v in req.items()}) == from_json(Token, FIXTURES["token"])
        assert sent[-1] == req


@pytest.mark.anyio
async def test_async_client_parses_the_fixtures() -> None:
    transport, sent = recorder(ROUTES)
    client = a12s.AsyncClient("http://x", "t", http=httpx.AsyncClient(transport=transport))
    assert await client.status() == from_json(Status, FIXTURES["statusResponse"], strict=True)
    assert await client.inbox("lead") == from_json(InboxList, FIXTURES["inboxListResponse"], strict=True)
    assert await client.services() == [from_json(ServiceStatus, FIXTURES["servicesResponse"]["services"][0])]
    req = FIXTURES["enqueueRequest"]
    await client.send("lead", req["body"], **{snake(k): v for k, v in req.items() if k != "body"})
    assert sent[-1] == req
    await client.create_token(**{snake(k): v for k, v in FIXTURES["tokenRequest"].items()})
    assert sent[-1] == FIXTURES["tokenRequest"]
