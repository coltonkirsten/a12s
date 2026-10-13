"""The object SDK: a Fleet hands out AgentComputer and Service objects you call methods on."""

from __future__ import annotations

import builtins
import os
from collections.abc import Callable, Sequence
from dataclasses import dataclass, fields
from typing import TYPE_CHECKING, Any, overload

import httpx

from . import _core as core
from ._client import Client, EventStream, TurnStream
from ._connect import _discover
from ._errors import A12sError
from ._fleetstream import FleetEventStream, FleetTurnStream, SharedStream, names_of
from ._parse import from_json
from ._spec import AgentComputerSpec, FleetSpec
from ._types import (
    Change,
    ComputerStatus,
    CredentialInfo,
    EnqueueResponse,
    Event,
    InboxList,
    Message,
    Reply,
    ServiceStatus,
    Status,
    Token,
    Turn,
)

if TYPE_CHECKING:
    from typing_extensions import Unpack

# Whole-Fleet writes retry this often when someone else wrote in between.
_REPLACE_TRIES = 5


def ac_path(name: str, suffix: str = "") -> str:
    return f"/v1/agent-computers/{core.enc(name)}{suffix}"


def service_path(name: str, suffix: str = "") -> str:
    return f"/v1/services/{core.enc(name)}{suffix}"


def write_headers(replace: bool) -> dict[str, str] | None:
    return None if replace else {"if-none-match": "*"}


@dataclass(frozen=True, slots=True, kw_only=True)
class FleetState:
    """The Fleet as written and its version. ``drift`` lists what changed through the API since the last a12s up."""

    version: int
    spec: FleetSpec
    drift: list[Change]


def fleet_state(data: Any) -> FleetState:
    return FleetState(
        version=int(data.get("version") or 0),
        spec=FleetSpec(data.get("fleet") or {}),
        drift=[from_json(Change, c) for c in data.get("drift") or []],
    )


class SentMessage(EnqueueResponse):
    """A queued Message: the fields of EnqueueResponse, and ``wait()`` for the Turn that delivers it."""

    __slots__ = ("_client", "_to")
    _client: Client
    _to: str

    def __init__(self, message: EnqueueResponse, client: Client, to: str) -> None:
        super().__init__(**{f.name: getattr(message, f.name) for f in fields(message)})
        object.__setattr__(self, "_client", client)
        object.__setattr__(self, "_to", to)

    def wait(self, *, on_event: Callable[[Event], None] | None = None) -> Reply:
        """Waits for the Turn that delivers this Message and returns it once it ends, also if it already has.

        ``on_event`` gets its events. A failed Turn is returned with status "failed"; a Message that is withdrawn,
        expired, replaced or held raises A12sError.
        """
        return self._client._reply(self._to, self, on_event)


class AgentComputer:
    """A handle on one agent computer. Methods always call the Controller; ``spec``, ``effective`` and ``status`` are
    a snapshot from the last create, get, reload or update."""

    def __init__(self, fleet: Fleet, data: dict[str, Any]) -> None:
        self._fleet = fleet
        self._set(data)

    def _set(self, data: dict[str, Any]) -> None:
        self.name: str = data["name"]
        self.spec: dict[str, Any] = data.get("spec") or {}
        self.effective: dict[str, Any] = data.get("effective") or {}
        self.status: ComputerStatus = from_json(ComputerStatus, data.get("status") or {"name": self.name})

    def __repr__(self) -> str:
        return f"<AgentComputer {self.name} {self.status.phase}>"

    def reload(self) -> AgentComputer:
        self._set(self._fleet._client._request("GET", ac_path(self.name)))
        return self

    def send(
        self,
        text: str,
        *,
        sender: str | None = None,
        metadata: dict[str, str] | None = None,
        fresh: bool = False,
        idempotency_key: str | None = None,
        expires: int | None = None,
        replace_key: str | None = None,
    ) -> SentMessage:
        """Queues ``text`` and returns at once; it is delivered at the next idle moment, even if busy or asleep.
        ``wait()`` on the result returns the Turn that delivers it."""
        message = self._fleet._client.send(
            self.name,
            text,
            sender=sender,
            metadata=metadata,
            fresh=fresh,
            idempotency_key=idempotency_key,
            expires=expires,
            replace_key=replace_key,
        )
        return SentMessage(message, self._fleet._client, self.name)

    def events(self, *, turn: str | None = None, on_gap: core.OnGap = "yield") -> FleetEventStream | EventStream:
        """Its events, live, from the Fleet's shared connection; ``turn`` replays one Turn from its start instead."""
        if turn is not None:
            return self._fleet._client.events(self.name, turn=turn, on_gap=on_gap)
        return FleetEventStream(self._fleet._shared, frozenset([self.name]), on_gap)

    @overload
    def turns(self, *, limit: None = None, on_gap: core.OnGap = "yield") -> FleetTurnStream: ...

    @overload
    def turns(self, *, limit: int, on_gap: core.OnGap = "yield") -> builtins.list[Turn]: ...

    def turns(
        self, *, limit: int | None = None, on_gap: core.OnGap = "yield"
    ) -> FleetTurnStream | builtins.list[Turn]:
        """Its Turns as they end, live; with ``limit``, its last finished Turns instead, newest first."""
        if limit is not None:
            return self._past_turns(limit)
        return FleetTurnStream(FleetEventStream(self._fleet._shared, frozenset([self.name]), on_gap))

    def _past_turns(self, limit: int) -> builtins.list[Turn]:
        res = self._fleet._client._request("GET", ac_path(self.name, f"/turns?limit={limit}"))
        return [core.turn_of(t) for t in (res or {}).get("turns") or []]

    def update(self, **changes: Unpack[AgentComputerSpec]) -> AgentComputer:
        """Changes the given fields (maps merge, lists replace). It applies at the next idle moment; the session
        continues."""
        res = self._fleet._client._request("PATCH", ac_path(self.name), dict(changes))
        core.warn_writes(res)
        if isinstance(res, dict) and isinstance(res.get("agentComputer"), dict):
            self._set(res["agentComputer"])
        return self

    def sleep(self, *, hold: bool = False) -> None:
        """Puts it to sleep; a sender in its wakeOnMessage wakes it. With ``hold``, it stays asleep until wake()."""
        self._fleet._client.sleep(self.name, hold=hold)

    def wake(self, *, clear_inbox: bool = False) -> None:
        """Wakes it; ``clear_inbox`` discards the pending Messages first."""
        self._fleet._client.wake(self.name, clear_inbox=clear_inbox)

    def hold(self, *, reason: str | None = None) -> InboxList:
        """Holds its inbox: Messages still queue, none is delivered until resume()."""
        return self._fleet._client.hold(self.name, reason=reason)

    def resume(self) -> InboxList:
        """Releases a hold on its inbox."""
        return self._fleet._client.resume(self.name)

    def stop(self, turn: str | None = None) -> None:
        """Interrupts a Turn (default: the active one)."""
        self._fleet._client.stop(self.name, turn)

    def inbox(self, *, state: str | None = None, bodies: bool = False, after: int | None = None) -> InboxList:
        return self._fleet._client.inbox(self.name, state=state, bodies=bodies, after=after)

    def message(self, id: str) -> Message:
        return self._fleet._client.message(self.name, id)

    def withdraw(self, id: str) -> Message:
        return self._fleet._client.withdraw(self.name, id)

    def remove(self) -> None:
        """Stops and removes it. Its drives stay."""
        self._fleet._client._request("DELETE", ac_path(self.name))


class AgentComputers:
    def __init__(self, fleet: Fleet) -> None:
        self._fleet = fleet

    def create(self, name: str, *, replace: bool = False, **spec: Unpack[AgentComputerSpec]) -> AgentComputer:
        """Creates an agent computer and returns once it is up, or once it waits in Provisioning for a credential with
        no value yet (an A12sWarning names it; it starts when the value is set). A taken name raises A12sError
        ``exists`` unless ``replace``."""
        res = self._fleet._client._request("PUT", ac_path(name), dict(spec), write_headers(replace))
        core.warn_writes(res)
        if isinstance(res, dict) and isinstance(res.get("agentComputer"), dict):
            return AgentComputer(self._fleet, res["agentComputer"])
        return self.get(name)

    def get(self, name: str) -> AgentComputer:
        """Raises a12s.NotFound when there is none."""
        return AgentComputer(self._fleet, self._fleet._client._request("GET", ac_path(name)))

    def list(self) -> builtins.list[AgentComputer]:
        res = self._fleet._client._request("GET", "/v1/agent-computers")
        return [AgentComputer(self._fleet, d) for d in (res or {}).get("agentComputers") or []]


class Service:
    """A handle on one Service; ``spec`` and ``status`` are a snapshot like AgentComputer's."""

    def __init__(self, fleet: Fleet, data: dict[str, Any]) -> None:
        self._fleet = fleet
        self._set(data)

    def _set(self, data: dict[str, Any]) -> None:
        self.status: ServiceStatus = from_json(ServiceStatus, data)
        self.name: str = self.status.name
        self.spec: dict[str, Any] = data.get("spec") or {}

    def __repr__(self) -> str:
        return f"<Service {self.name} {self.status.phase}>"

    def reload(self) -> Service:
        self._set(self._fleet._client._request("GET", service_path(self.name)))
        return self

    def update(self, **changes: Any) -> Service:
        res = self._fleet._client._request("PATCH", service_path(self.name), changes)
        core.warn_writes(res)
        if isinstance(res, dict) and isinstance(res.get("service"), dict):
            self._set(res["service"])
        return self

    def sleep(self) -> None:
        self._set(self._fleet._client._request("POST", service_path(self.name, "/power"), {"state": "asleep"}))

    def wake(self) -> None:
        self._set(self._fleet._client._request("POST", service_path(self.name, "/power"), {"state": "awake"}))

    def remove(self) -> None:
        """Stops and removes it. Its state drive stays."""
        self._fleet._client._request("DELETE", service_path(self.name))


class Services:
    def __init__(self, fleet: Fleet) -> None:
        self._fleet = fleet

    def create(self, name: str, *, replace: bool = False, **spec: Any) -> Service:
        res = self._fleet._client._request("PUT", service_path(name), spec, write_headers(replace))
        core.warn_writes(res)
        if isinstance(res, dict) and isinstance(res.get("service"), dict):
            return Service(self._fleet, res["service"])
        return self.get(name)

    def get(self, name: str) -> Service:
        return Service(self._fleet, self._fleet._client._request("GET", service_path(name)))

    def list(self) -> builtins.list[Service]:
        res = self._fleet._client._request("GET", "/v1/services")
        return [Service(self._fleet, d) for d in (res or {}).get("services") or []]


class Tokens:
    def __init__(self, fleet: Fleet) -> None:
        self._fleet = fleet

    def create(
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
        return from_json(Token, self._fleet._client._request("POST", "/v1/tokens", req))

    def list(self) -> builtins.list[Token]:
        res = self._fleet._client._request("GET", "/v1/tokens")
        return [from_json(Token, t) for t in (res or {}).get("tokens") or []]

    def remove(self, name: str) -> None:
        self._fleet._client._request("DELETE", f"/v1/tokens/{core.enc(name)}")


class Credentials:
    """Credentials by name: set once, then name them in a spec's ``credentials``."""

    def __init__(self, fleet: Fleet) -> None:
        self._fleet = fleet

    def set(self, name: str, value: str) -> CredentialInfo:
        """Stores the value on the Controller; agent computers that use it get the new value at once."""
        path = f"/v1/credentials/{core.enc(name)}"
        return from_json(CredentialInfo, self._fleet._client._request("PUT", path, {"value": value}))

    def list(self) -> builtins.list[CredentialInfo]:
        res = self._fleet._client._request("GET", "/v1/credentials")
        return [from_json(CredentialInfo, c) for c in (res or {}).get("credentials") or []]

    def remove(self, name: str) -> None:
        """Raises A12sError ``in_use`` while an agent computer or Service uses it."""
        self._fleet._client._request("DELETE", f"/v1/credentials/{core.enc(name)}")


class Fleet:
    """A running Fleet. It connects like ``connect()``: ``url`` and ``token``, else ``$A12S_URL`` and
    ``$A12S_TOKEN``, else the nearest fleet.yaml's Fleet (or ``fleet=``), else the only running Controller.

    ``fleet.agent_computers``, ``fleet.services`` and ``fleet.tokens`` hand out objects; ``with a12s.Fleet() as
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
        http: httpx.Client | None = None,
    ) -> None:
        self._client = Client(*_discover(fleet, url, token, home, cwd), http=http)
        self._shared = SharedStream(self._client)
        self.url = self._client.url
        self.agent_computers = AgentComputers(self)
        self.services = Services(self)
        self.tokens = Tokens(self)
        self.credentials = Credentials(self)

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> Fleet:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def status(self) -> Status:
        return self._client.status()

    def events(self, agent_computers: str | Sequence[str] | None = None, *, on_gap: core.OnGap = "yield") -> FleetEventStream:
        """Live events of every agent computer (or those named), and the Fleet's own (fleet.changed,
        agent_computer.phase), on the Fleet's one shared connection."""
        return FleetEventStream(self._shared, names_of(agent_computers), on_gap)

    def turns(self, agent_computers: str | Sequence[str] | None = None, *, on_gap: core.OnGap = "yield") -> FleetTurnStream:
        """Turns of every agent computer (or those named), as they end."""
        return FleetTurnStream(self.events(agent_computers, on_gap=on_gap))

    def get(self) -> FleetState:
        """The whole Fleet as written, and its version."""
        return fleet_state(self._client._request("GET", "/v1/fleet"))

    def replace(self, fn: Callable[[FleetSpec], FleetSpec | None]) -> FleetState:
        """Reads the Fleet, runs ``fn`` on it (edit it in place, or return a new one) and writes it back, retrying when
        someone else wrote in between."""
        for attempt in range(_REPLACE_TRIES):
            state = self.get()
            spec = fn(state.spec)
            body = {"fleet": dict(spec if spec is not None else state.spec)}
            try:
                core.warn_writes(self._client._request("PUT", "/v1/fleet", body, {"if-match": f'"{state.version}"'}))
            except A12sError as err:
                if err.code == "version_mismatch" and attempt < _REPLACE_TRIES - 1:
                    continue
                raise
            return self.get()
        raise AssertionError("unreachable")
