"""One GET /v1/events connection per Fleet, shared by every events() and turns() on it: watching fifty agent
computers takes one connection, not fifty."""

from __future__ import annotations

import asyncio
import json
import queue
import threading
from datetime import datetime, timezone
from types import TracebackType
from typing import TYPE_CHECKING, Any, Literal, Union

import httpx

from . import _core as core
from ._errors import A12sGapError, A12sStreamError
from ._parse import from_json
from ._sse import SSEDecoder, SSEFrame
from ._types import Event, Turn

if TYPE_CHECKING:
    from ._async import AsyncClient
    from ._client import Client

_Item = Union[tuple[Literal["event"], Event], tuple[Literal["error"], BaseException], tuple[Literal["wake"], None]]


def _gap_event() -> Event:
    ts = datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")
    return Event(v=1, agent_computer="", session="", turn="", seq=0, ts=ts, type="gap", data={})


def _wanted(names: frozenset[str] | None, ev: Event) -> bool:
    if names is None:
        return True
    if ev.type == "fleet.changed":
        changed = ev.data.get("names") if isinstance(ev.data, dict) else None
        return isinstance(changed, list) and any(n in names for n in changed)
    if ev.type == "service.phase":
        return isinstance(ev.data, dict) and ev.data.get("service") in names
    return ev.agent_computer in names


class _Reader:
    """The cursor across reconnects and what each frame of the fleet-wide stream means."""

    def __init__(self) -> None:
        self.cursor: str | None = None

    def headers(self, token: str | None) -> dict[str, str]:
        extra = {"accept": "text/event-stream"}
        if self.cursor is not None:
            extra["last-event-id"] = self.cursor
        return core.headers(token, extra)

    def frame(self, f: SSEFrame) -> tuple[str, Event | None]:
        """("head" | "gap" | "event" | "skip", the event)."""
        if f.event == "head":
            return "head", None
        if f.event == "gap":
            return "gap", _gap_event()
        if f.event or not f.data:
            return "skip", None
        try:
            raw = json.loads(f.data)
            if not isinstance(raw, dict):
                raise ValueError("not an object")
            ev = from_json(Event, raw)
        except ValueError as err:
            raise A12sStreamError(f"malformed event after {self.cursor or '(start)'}: {f.data[:120]}") from err
        if f.id is not None:
            self.cursor = f.id
        return "event", ev


class _Sub:
    def __init__(self, names: frozenset[str] | None, on_gap: core.OnGap) -> None:
        self.names = names
        self.on_gap = on_gap
        self.queue: queue.SimpleQueue[_Item] = queue.SimpleQueue()


class SharedStream:
    """The Fleet's one connection: a thread runs it while anything is subscribed."""

    def __init__(self, client: Client, reconnect: float = 1.0) -> None:
        self._client = client
        self._reconnect = reconnect
        self._lock = threading.Lock()
        self._subs: set[_Sub] = set()
        self._stop: threading.Event | None = None
        self._live = threading.Event()
        self._response: httpx.Response | None = None

    def subscribe(self, names: frozenset[str] | None, on_gap: core.OnGap) -> _Sub:
        sub = _Sub(names, on_gap)
        with self._lock:
            self._subs.add(sub)
            if self._stop is None:
                self._stop = threading.Event()
                self._live.clear()
                threading.Thread(target=self._run, args=(self._stop,), name="a12s-fleet-events", daemon=True).start()
        return sub

    def unsubscribe(self, sub: _Sub) -> None:
        with self._lock:
            self._subs.discard(sub)
            if self._subs or self._stop is None:
                return
            self._stop.set()
            self._stop = None
            res = self._response
        if res is not None:
            try:
                res.close()
            except Exception:
                pass

    def wait_live(self) -> None:
        self._live.wait()

    def _deliver(self, ev: Event, gap: bool = False) -> None:
        with self._lock:
            subs = list(self._subs)
        for s in subs:
            if gap and s.on_gap == "throw":
                s.queue.put(("error", A12sGapError({})))
            elif gap or _wanted(s.names, ev):
                s.queue.put(("event", ev))

    def _fail(self, err: BaseException) -> None:
        with self._lock:
            subs = list(self._subs)
        self._live.set()
        for s in subs:
            s.queue.put(("error", err))

    def _run(self, stop: threading.Event) -> None:
        reader = _Reader()
        url = self._client.url + "/v1/events"
        http = self._client._http
        while not stop.is_set():
            try:
                with http.stream("GET", url, headers=reader.headers(self._client._token)) as res:
                    with self._lock:
                        self._response = res
                    if not res.is_success:
                        res.read()
                        self._fail(core.api_error(res.status_code, res.text))
                        return
                    decoder = SSEDecoder()
                    try:
                        for chunk in res.iter_bytes():
                            for f in decoder.feed(chunk):
                                kind, ev = reader.frame(f)
                                if kind == "head":
                                    self._live.set()
                                elif ev is not None:
                                    self._deliver(ev, gap=kind == "gap")
                    except httpx.TransportError:
                        pass
            except httpx.TransportError:
                pass
            except A12sStreamError as err:
                self._fail(err)
                return
            stop.wait(self._reconnect)


class FleetEventStream:
    """Events from the Fleet's shared connection, filtered to some agent computers or none.

    ``with fleet.events() as events:`` waits until the stream is live (nothing after that is missed) and closes it.
    """

    def __init__(self, shared: SharedStream, names: frozenset[str] | None, on_gap: core.OnGap) -> None:
        self._shared = shared
        self._names = names
        self._on_gap = on_gap
        self._sub: _Sub | None = None
        self._closed = False

    def _start(self) -> _Sub:
        if self._sub is None:
            self._sub = self._shared.subscribe(self._names, self._on_gap)
        return self._sub

    def ready(self) -> None:
        self._start()
        self._shared.wait_live()

    def close(self) -> None:
        self._closed = True
        if self._sub is not None:
            self._sub.queue.put(("wake", None))
            self._shared.unsubscribe(self._sub)

    def __iter__(self) -> FleetEventStream:
        return self

    def __next__(self) -> Event:
        sub = self._start()
        while not self._closed:
            item = sub.queue.get()
            if item[0] == "event":
                return item[1]
            if item[0] == "error":
                self.close()
                raise item[1]
        raise StopIteration

    def __enter__(self) -> FleetEventStream:
        try:
            self.ready()
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        self.close()


class FleetTurnStream:
    """Turns as they end, folded from a FleetEventStream. Same ``ready()``, ``close()`` and ``with``."""

    def __init__(self, events: FleetEventStream) -> None:
        self._events = events
        self._builder = core.TurnBuilder()

    def ready(self) -> None:
        self._events.ready()

    def close(self) -> None:
        self._events.close()

    def __iter__(self) -> FleetTurnStream:
        return self

    def __next__(self) -> Turn:
        for ev in self._events:
            turn = self._builder.add(ev)
            if turn is not None:
                return turn
        raise StopIteration

    def __enter__(self) -> FleetTurnStream:
        self._events.__enter__()
        return self

    def __exit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        self.close()


class _AsyncSub:
    def __init__(self, names: frozenset[str] | None, on_gap: core.OnGap) -> None:
        self.names = names
        self.on_gap = on_gap
        self.queue: asyncio.Queue[_Item] = asyncio.Queue()


class AsyncSharedStream:
    """SharedStream for asyncio: one task runs the connection while anything is subscribed."""

    def __init__(self, client: AsyncClient, reconnect: float = 1.0) -> None:
        self._client = client
        self._reconnect = reconnect
        self._subs: set[_AsyncSub] = set()
        self._task: asyncio.Task[None] | None = None
        self._live = asyncio.Event()

    def subscribe(self, names: frozenset[str] | None, on_gap: core.OnGap) -> _AsyncSub:
        sub = _AsyncSub(names, on_gap)
        self._subs.add(sub)
        if self._task is None:
            self._live.clear()
            self._task = asyncio.get_running_loop().create_task(self._run())
        return sub

    def unsubscribe(self, sub: _AsyncSub) -> None:
        self._subs.discard(sub)
        if not self._subs and self._task is not None:
            self._task.cancel()
            self._task = None

    async def wait_live(self) -> None:
        await self._live.wait()

    def _deliver(self, ev: Event, gap: bool = False) -> None:
        for s in list(self._subs):
            if gap and s.on_gap == "throw":
                s.queue.put_nowait(("error", A12sGapError({})))
            elif gap or _wanted(s.names, ev):
                s.queue.put_nowait(("event", ev))

    def _fail(self, err: BaseException) -> None:
        self._live.set()
        for s in list(self._subs):
            s.queue.put_nowait(("error", err))

    async def _run(self) -> None:
        reader = _Reader()
        url = self._client.url + "/v1/events"
        http = self._client._http
        while True:
            try:
                async with http.stream("GET", url, headers=reader.headers(self._client._token)) as res:
                    if not res.is_success:
                        await res.aread()
                        self._fail(core.api_error(res.status_code, res.text))
                        return
                    decoder = SSEDecoder()
                    try:
                        async for chunk in res.aiter_bytes():
                            for f in decoder.feed(chunk):
                                kind, ev = reader.frame(f)
                                if kind == "head":
                                    self._live.set()
                                elif ev is not None:
                                    self._deliver(ev, gap=kind == "gap")
                    except httpx.TransportError:
                        pass
            except httpx.TransportError:
                pass
            except A12sStreamError as err:
                self._fail(err)
                return
            await asyncio.sleep(self._reconnect)


class AsyncFleetEventStream:
    """FleetEventStream for asyncio: ``async with``, ``async for``, ``await ready()``."""

    def __init__(self, shared: AsyncSharedStream, names: frozenset[str] | None, on_gap: core.OnGap) -> None:
        self._shared = shared
        self._names = names
        self._on_gap = on_gap
        self._sub: _AsyncSub | None = None
        self._closed = False

    def _start(self) -> _AsyncSub:
        if self._sub is None:
            self._sub = self._shared.subscribe(self._names, self._on_gap)
        return self._sub

    async def ready(self) -> None:
        self._start()
        await self._shared.wait_live()

    def close(self) -> None:
        self._closed = True
        if self._sub is not None:
            self._sub.queue.put_nowait(("wake", None))
            self._shared.unsubscribe(self._sub)

    async def aclose(self) -> None:
        self.close()

    def __aiter__(self) -> AsyncFleetEventStream:
        return self

    async def __anext__(self) -> Event:
        sub = self._start()
        while not self._closed:
            item = await sub.queue.get()
            if item[0] == "event":
                return item[1]
            if item[0] == "error":
                self.close()
                raise item[1]
        raise StopAsyncIteration

    async def __aenter__(self) -> AsyncFleetEventStream:
        try:
            await self.ready()
        except BaseException:
            self.close()
            raise
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        self.close()


class AsyncFleetTurnStream:
    def __init__(self, events: AsyncFleetEventStream) -> None:
        self._events = events
        self._builder = core.TurnBuilder()

    async def ready(self) -> None:
        await self._events.ready()

    def close(self) -> None:
        self._events.close()

    async def aclose(self) -> None:
        self.close()

    def __aiter__(self) -> AsyncFleetTurnStream:
        return self

    async def __anext__(self) -> Turn:
        async for ev in self._events:
            turn = self._builder.add(ev)
            if turn is not None:
                return turn
        raise StopAsyncIteration

    async def __aenter__(self) -> AsyncFleetTurnStream:
        await self._events.__aenter__()
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        self.close()


def names_of(names: str | Any | None) -> frozenset[str] | None:
    if names is None:
        return None
    if isinstance(names, str):
        return frozenset([names])
    return frozenset(names)

