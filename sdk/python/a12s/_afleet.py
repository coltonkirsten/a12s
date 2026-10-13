"""The asyncio object SDK: AsyncFleet, the same shape as AsyncFleet with every call awaited."""

from __future__ import annotations

import builtins
import os
from collections.abc import Awaitable, Callable, Sequence
from typing import TYPE_CHECKING, Any

import httpx

from . import _core as core
from ._async import AsyncClient, AsyncEventStream, AsyncTurnStream
from ._connect import _discover
from ._errors import A12sError
from ._parse import from_json
from ._spec import AgentComputerSpec, FleetSpec
from ._types import (
    ComputerStatus,
    CredentialInfo,
    EnqueueResponse,
    Event,
    InboxList,
    Message,
    PromptResult,
    ServiceStatus,
    Status,
    Token,
)

from ._fleet import _REPLACE_TRIES, FleetState, ac_path, fleet_state, service_path, write_headers

if TYPE_CHECKING:
    from typing_extensions import Unpack


class AsyncAgentComputer:
    """A handle on one agent computer. Methods always call the Controller; ``spec``, ``effective`` and ``status`` are
    a snapshot from the last create, get, reload or update."""

    def __init__(self, fleet: AsyncFleet, data: dict[str, Any]) -> None:
        self._fleet = fleet
        self._set(data)

    def _set(self, data: dict[str, Any]) -> None:
        self.name: str = data["name"]
        self.spec: dict[str, Any] = data.get("spec") or {}
        self.effective: dict[str, Any] = data.get("effective") or {}
        self.status: ComputerStatus = from_json(ComputerStatus, data.get("status") or {"name": self.name})

    def __repr__(self) -> str:
        return f"<AsyncAgentComputer {self.name} {self.status.phase}>"

    async def reload(self) -> AsyncAgentComputer:
        self._set(await self._fleet._client._request("GET", ac_path(self.name)))
        return self

    async def prompt(
        self,
        text: str,
        *,
        on_event: Callable[[Event], None] | None = None,
        sender: str | None = None,
        metadata: dict[str, str] | None = None,
        fresh: bool = False,
        idempotency_key: str | None = None,
        expires: int | None = None,
        replace_key: str | None = None,
    ) -> PromptResult:
        """Sends ``text`` and returns the Turn that delivers it, once it ends. ``on_event`` gets its events live."""
        return await self._fleet._client.prompt(
            self.name,
            text,
            on_event=on_event,
            sender=sender,
            metadata=metadata,
            fresh=fresh,
            idempotency_key=idempotency_key,
            expires=expires,
            replace_key=replace_key,
        )

    async def send(
        self,
        text: str,
        *,
        sender: str | None = None,
        metadata: dict[str, str] | None = None,
        fresh: bool = False,
        idempotency_key: str | None = None,
        expires: int | None = None,
        replace_key: str | None = None,
    ) -> EnqueueResponse:
        """Queues ``text`` and returns at once; it is delivered at the next idle moment, even if busy or asleep."""
        return await self._fleet._client.send(
            self.name,
            text,
            sender=sender,
            metadata=metadata,
            fresh=fresh,
            idempotency_key=idempotency_key,
            expires=expires,
            replace_key=replace_key,
        )

    def events(
        self, *, turn: str | None = None, after: int | None = None, reconnect: float = 1.0, on_gap: core.OnGap = "yield"
    ) -> AsyncEventStream:
        """Its events, live; ``turn`` replays one Turn from its start."""
        return self._fleet._client.events(self.name, turn=turn, after=after, reconnect=reconnect, on_gap=on_gap)

    def turns(self, *, reconnect: float = 1.0, on_gap: core.OnGap = "yield") -> AsyncTurnStream:
        """Its Turns, live, as they end."""
        return self._fleet._client.turns(self.name, reconnect=reconnect, on_gap=on_gap)

    async def update(self, **changes: Unpack[AgentComputerSpec]) -> AsyncAgentComputer:
        """Changes the given fields (maps merge, lists replace). It applies at the next idle moment; the session
        continues."""
        res = await self._fleet._client._request("PATCH", ac_path(self.name), dict(changes))
        if isinstance(res, dict) and isinstance(res.get("agentComputer"), dict):
            self._set(res["agentComputer"])
        return self

    async def sleep(self) -> None:
        await self._fleet._client.sleep(self.name)

    async def wake(self) -> None:
        await self._fleet._client.wake(self.name)

    async def stop(self, turn: str | None = None) -> None:
        """Interrupts a Turn (default: the active one)."""
        await self._fleet._client.stop(self.name, turn)

    async def inbox(self, *, state: str | None = None, bodies: bool = False, after: int | None = None) -> InboxList:
        return await self._fleet._client.inbox(self.name, state=state, bodies=bodies, after=after)

    async def message(self, id: str) -> Message:
        return await self._fleet._client.message(self.name, id)

    async def withdraw(self, id: str) -> Message:
        return await self._fleet._client.withdraw(self.name, id)

    async def remove(self) -> None:
        """Stops and removes it. Its drives stay."""
        await self._fleet._client._request("DELETE", ac_path(self.name))


class AsyncAgentComputers:
    def __init__(self, fleet: AsyncFleet) -> None:
        self._fleet = fleet

    async def create(self, name: str, *, replace: bool = False, **spec: Unpack[AgentComputerSpec]) -> AsyncAgentComputer:
        """Creates an agent computer and returns once it is up. A taken name raises A12sError ``exists`` unless
        ``replace``."""
        res = await self._fleet._client._request("PUT", ac_path(name), dict(spec), write_headers(replace))
        if isinstance(res, dict) and isinstance(res.get("agentComputer"), dict):
            return AsyncAgentComputer(self._fleet, res["agentComputer"])
        return await self.get(name)

    async def get(self, name: str) -> AsyncAgentComputer:
        """Raises a12s.NotFound when there is none."""
        return AsyncAgentComputer(self._fleet, await self._fleet._client._request("GET", ac_path(name)))

    async def list(self) -> builtins.list[AsyncAgentComputer]:
        res = await self._fleet._client._request("GET", "/v1/agent-computers")
        return [AsyncAgentComputer(self._fleet, d) for d in (res or {}).get("agentComputers") or []]


class AsyncService:
    """A handle on one AsyncService; ``spec`` and ``status`` are a snapshot like AsyncAgentComputer's."""

    def __init__(self, fleet: AsyncFleet, data: dict[str, Any]) -> None:
        self._fleet = fleet
        self._set(data)

    def _set(self, data: dict[str, Any]) -> None:
        self.status: ServiceStatus = from_json(ServiceStatus, data)
        self.name: str = self.status.name
        self.spec: dict[str, Any] = data.get("spec") or {}

    def __repr__(self) -> str:
        return f"<AsyncService {self.name} {self.status.phase}>"

    async def reload(self) -> AsyncService:
        self._set(await self._fleet._client._request("GET", service_path(self.name)))
        return self

    async def update(self, **changes: Any) -> AsyncService:
        res = await self._fleet._client._request("PATCH", service_path(self.name), changes)
        if isinstance(res, dict) and isinstance(res.get("service"), dict):
            self._set(res["service"])
        return self

    async def sleep(self) -> None:
        self._set(await self._fleet._client._request("POST", service_path(self.name, "/power"), {"state": "asleep"}))

    async def wake(self) -> None:
        self._set(await self._fleet._client._request("POST", service_path(self.name, "/power"), {"state": "awake"}))

    async def remove(self) -> None:
        """Stops and removes it. Its state drive stays."""
        await self._fleet._client._request("DELETE", service_path(self.name))


class AsyncServices:
    def __init__(self, fleet: AsyncFleet) -> None:
        self._fleet = fleet

    async def create(self, name: str, *, replace: bool = False, **spec: Any) -> AsyncService:
        res = await self._fleet._client._request("PUT", service_path(name), spec, write_headers(replace))
        if isinstance(res, dict) and isinstance(res.get("service"), dict):
            return AsyncService(self._fleet, res["service"])
        return await self.get(name)

    async def get(self, name: str) -> AsyncService:
        return AsyncService(self._fleet, await self._fleet._client._request("GET", service_path(name)))

    async def list(self) -> builtins.list[AsyncService]:
        res = await self._fleet._client._request("GET", "/v1/services")
        return [AsyncService(self._fleet, d) for d in (res or {}).get("services") or []]


class AsyncTokens:
    def __init__(self, fleet: AsyncFleet) -> None:
        self._fleet = fleet

    async def create(
        self,
        name: str,
        *,
        prefix: str | None = None,
        actions: Sequence[str] | None = None,
        agent_computers: Sequence[str] | None = None,
    ) -> Token:
        """A token for another program; the secret is in the result once.

        With ``prefix`` it may do everything the owner can for agent computers, Services and credentials whose names
        start with it, and nothing outside; otherwise it may do ``actions`` on ``agent_computers``.
        """
        req: dict[str, Any] = {"name": name}
        if prefix is not None:
            req["prefix"] = prefix
        if actions is not None:
            req["actions"] = list(actions)
        if agent_computers is not None:
            req["computers"] = list(agent_computers)
        return from_json(Token, await self._fleet._client._request("POST", "/v1/tokens", req))

    async def list(self) -> builtins.list[Token]:
        res = await self._fleet._client._request("GET", "/v1/tokens")
        return [from_json(Token, t) for t in (res or {}).get("tokens") or []]

    async def remove(self, name: str) -> None:
        await self._fleet._client._request("DELETE", f"/v1/tokens/{core.enc(name)}")


class AsyncCredentials:
    """Credentials by name: set once, then name them in a spec's ``credentials``."""

    def __init__(self, fleet: AsyncFleet) -> None:
        self._fleet = fleet

    async def set(self, name: str, value: str) -> CredentialInfo:
        """Stores the value on the Controller; agent computers that use it get the new value at once."""
        path = f"/v1/credentials/{core.enc(name)}"
        return from_json(CredentialInfo, await self._fleet._client._request("PUT", path, {"value": value}))

    async def list(self) -> builtins.list[CredentialInfo]:
        res = await self._fleet._client._request("GET", "/v1/credentials")
        return [from_json(CredentialInfo, c) for c in (res or {}).get("credentials") or []]

    async def remove(self, name: str) -> None:
        """Raises A12sError ``in_use`` while an agent computer or Service uses it."""
        await self._fleet._client._request("DELETE", f"/v1/credentials/{core.enc(name)}")


class AsyncFleet:
    """A running AsyncFleet. It connects like ``connect()``: ``url`` and ``token``, else ``$A12S_URL`` and
    ``$A12S_TOKEN``, else the nearest fleet.yaml's AsyncFleet (or ``fleet=``), else the only running Controller.

    ``fleet.agent_computers``, ``fleet.services`` and ``fleet.tokens`` hand out objects; ``async with a12s.AsyncFleet() as
    fleet:`` closes it.
    """

    def __init__(
        self,
        url: str | None = None,
        token: str | None = None,
        *,
        fleet: str | None = None,
        home: str | os.PathLike[str] | None = None,
        cwd: str | os.PathLike[str] | None = None,
        http: httpx.AsyncClient | None = None,
    ) -> None:
        self._client = AsyncClient(*_discover(fleet, url, token, home, cwd), http=http)
        self.url = self._client.url
        self.agent_computers = AsyncAgentComputers(self)
        self.services = AsyncServices(self)
        self.tokens = AsyncTokens(self)
        self.credentials = AsyncCredentials(self)

    async def aclose(self) -> None:
        await self._client.aclose()

    async def __aenter__(self) -> AsyncFleet:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def status(self) -> Status:
        return await self._client.status()

    def events(
        self,
        agent_computers: str | Sequence[str] | None = None,
        *,
        reconnect: float = 1.0,
        on_gap: core.OnGap = "yield",
    ) -> AsyncEventStream:
        """Live events of every agent computer (or those named), merged."""
        return self._client.events(agent_computers, reconnect=reconnect, on_gap=on_gap)

    def turns(
        self,
        agent_computers: str | Sequence[str] | None = None,
        *,
        reconnect: float = 1.0,
        on_gap: core.OnGap = "yield",
    ) -> AsyncTurnStream:
        """Turns of every agent computer (or those named), as they end."""
        return self._client.turns(agent_computers, reconnect=reconnect, on_gap=on_gap)

    async def get(self) -> FleetState:
        """The whole AsyncFleet as written, and its version."""
        return fleet_state(await self._client._request("GET", "/v1/fleet"))

    async def replace(self, fn: Callable[[FleetSpec], FleetSpec | None | Awaitable[FleetSpec | None]]) -> FleetState:
        """Reads the AsyncFleet, runs ``fn`` on it (edit it in place, or return a new one) and writes it back, retrying when
        someone else wrote in between."""
        for attempt in range(_REPLACE_TRIES):
            state = await self.get()
            spec = fn(state.spec)
            if isinstance(spec, Awaitable):
                spec = await spec
            body = {"fleet": dict(spec if spec is not None else state.spec)}
            try:
                await self._client._request("PUT", "/v1/fleet", body, {"if-match": f'"{state.version}"'})
            except A12sError as err:
                if err.code == "version_mismatch" and attempt < _REPLACE_TRIES - 1:
                    continue
                raise
            return await self.get()
        raise AssertionError("unreachable")
