"""The object SDK against a fake Controller: the routes, headers and bodies each call sends."""

from __future__ import annotations

import json
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
        if req.method == "DELETE" and path == "/v1/agent-computers/hello":
            return httpx.Response(200, json={"version": self.version, "removed": ["hello"]})
        if req.method == "GET" and path == "/v1/fleet":
            return httpx.Response(200, json={"version": self.version, "fleet": {"agent_computers": {}}, "drift": []})
        if req.method == "PUT" and path == "/v1/fleet":
            if self.conflicts:
                self.conflicts -= 1
                self.version += 1
                return httpx.Response(412, json={"code": "version_mismatch", "message": "stale"})
            return httpx.Response(200, json={"version": self.version + 1})
        return httpx.Response(500, json={"code": "unexpected", "message": f"{req.method} {path}"})


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
    await hello.remove()
    assert fake.seen[-1][:2] == ("DELETE", "/v1/agent-computers/hello")
    await fleet.aclose()
