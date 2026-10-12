from __future__ import annotations

import asyncio
from collections.abc import AsyncGenerator, Awaitable, Callable, Sequence
from contextlib import aclosing
from types import TracebackType
from typing import Any, Literal, Union

import httpx

from . import _core as core
from ._errors import A12sError, A12sStreamError
from ._parse import from_json
from ._sse import SSEDecoder
from ._types import (
    ComputerStatus,
    EnqueueResponse,
    Event,
    InboxList,
    Message,
    PromptResult,
    ServiceStatus,
    Status,
    Token,
    Turn,
)


class AsyncClient:
    """The asyncio twin of Client: the same methods, awaited."""

    def __init__(self, url: str, token: str | None = None, *, http: httpx.AsyncClient | None = None) -> None:
        self.url = url.rstrip("/")
        self._token = token
        self._owns_http = http is None
        self._http = http if http is not None else httpx.AsyncClient(timeout=core.TIMEOUT)

    async def aclose(self) -> None:
        if self._owns_http:
            await self._http.aclose()

    async def __aenter__(self) -> AsyncClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def _request(self, method: str, path: str, body: Any = None) -> Any:
        res = await self._http.request(method, self.url + path, **core.json_request(self._token, body))
        return core.decode(res)

    async def status(self) -> Status:
        """Every Computer and Service: phase, session, active Turn, inbox, power."""
        return from_json(Status, await self._request("GET", "/v1/status"))

    async def computer(self, name: str) -> ComputerStatus:
        for c in (await self.status()).computers:
            if c.name == name:
                return c
        raise A12sError(404, "not_found", f"no Computer {name}")

    async def send(
        self,
        computer: str,
        body: str,
        *,
        sender: str | None = None,
        metadata: dict[str, str] | None = None,
        fresh: bool = False,
        idempotency_key: str | None = None,
        expires: int | None = None,
        replace_key: str | None = None,
    ) -> EnqueueResponse:
        """See Client.send."""
        m = core.enqueue_body(body, sender, metadata, fresh, idempotency_key, expires, replace_key)
        return from_json(EnqueueResponse, await self._request("POST", f"/v1/computers/{core.enc(computer)}/inbox", m))

    async def prompt(
        self,
        computer: str,
        body: str,
        *,
        on_event: Callable[[Event], None] | None = None,
        sender: str | None = None,
        metadata: dict[str, str] | None = None,
        fresh: bool = False,
        idempotency_key: str | None = None,
        expires: int | None = None,
        replace_key: str | None = None,
    ) -> PromptResult:
        """See Client.prompt."""
        message = await self.send(
            computer,
            body,
            sender=sender,
            metadata=metadata,
            fresh=fresh,
            idempotency_key=idempotency_key,
            expires=expires,
            replace_key=replace_key,
        )
        turn = await self._delivered_in(computer, message.id)
        builder = core.TurnBuilder()
        # A gap mid-Turn would silently drop text: fail loudly instead.
        state = core.StreamState(computer, None, "throw")
        async with aclosing(self._stream(core.events_path(computer, turn), state, 1.0)) as events:
            async for ev in events:
                if on_event is not None:
                    on_event(ev)
                done = builder.add(ev)
                if done is not None and done.id == turn:
                    return core.prompt_result(done, message)
        raise A12sStreamError(f"event stream for turn {turn} ended early")

    def events(
        self,
        computers: str | Sequence[str] | None = None,
        *,
        turn: str | None = None,
        after: int | None = None,
        reconnect: float = 1.0,
        on_gap: core.OnGap = "yield",
    ) -> AsyncEventStream:
        """See Client.events."""
        names = core.stream_names(computers, turn, after)

        def source(name: str, live: Callable[[], None]) -> AsyncGenerator[Event, None]:
            state = core.StreamState(name, after, on_gap, live)
            return self._stream(core.events_path(name, turn), state, reconnect)

        async def all_names() -> list[str]:
            return [c.name for c in (await self.status()).computers]

        return AsyncEventStream(source, names if names is not None else all_names)

    def turns(
        self,
        computers: str | Sequence[str] | None = None,
        *,
        reconnect: float = 1.0,
        on_gap: core.OnGap = "yield",
    ) -> AsyncTurnStream:
        """See Client.turns."""
        return AsyncTurnStream(self.events(computers, reconnect=reconnect, on_gap=on_gap))

    async def inbox(
        self, computer: str, *, state: str | None = None, bodies: bool = False, after: int | None = None
    ) -> InboxList:
        return from_json(InboxList, await self._request("GET", core.inbox_path(computer, state, bodies, after)))

    async def message(self, computer: str, id: str) -> Message:
        path = f"/v1/computers/{core.enc(computer)}/inbox/{core.enc(id)}"
        return from_json(Message, await self._request("GET", path))

    async def withdraw(self, computer: str, id: str) -> Message:
        """Withdraws a pending Message; a 409 A12sError names its state if it is too late."""
        path = f"/v1/computers/{core.enc(computer)}/inbox/{core.enc(id)}"
        return from_json(Message, await self._request("DELETE", path))

    async def stop(self, computer: str, turn: str | None = None) -> None:
        """Interrupts a Turn (default: the active one). The Turn ends ``interrupted``; nothing is replayed."""
        id = turn if turn is not None else (await self.computer(computer)).active_turn
        if id:
            await self._request("POST", f"/v1/computers/{core.enc(computer)}/turns/{core.enc(id)}/stop", {})

    async def sleep(self, computer: str) -> None:
        """Stops the Computer's processes and keeps its disks, session and inbox."""
        await self._request("POST", f"/v1/computers/{core.enc(computer)}/power", {"state": "asleep"})

    async def wake(self, computer: str) -> None:
        """Starts a sleeping Computer; its session resumes on the next Turn."""
        await self._request("POST", f"/v1/computers/{core.enc(computer)}/power", {"state": "awake"})

    async def services(self) -> list[ServiceStatus]:
        res = await self._request("GET", "/v1/services")
        return [from_json(ServiceStatus, s) for s in (res or {}).get("services") or []]

    async def service(self, name: str) -> ServiceStatus:
        return from_json(ServiceStatus, await self._request("GET", f"/v1/services/{core.enc(name)}"))

    async def create_token(self, name: str, actions: Sequence[str], computers: Sequence[str]) -> Token:
        """A scoped token for another program; owner only. The secret is in the response once."""
        req = {"name": name, "actions": list(actions), "computers": list(computers)}
        return from_json(Token, await self._request("POST", "/v1/tokens", req))

    async def put_fleet(self, request: dict[str, Any]) -> dict[str, Any]:
        """What ``a12s up`` sends: validate and apply a whole Fleet. Most apps leave this to the CLI."""
        res: dict[str, Any] = await self._request("PUT", "/v1/fleet", request)
        return res

    async def _delivered_in(self, computer: str, id: str) -> str:
        # Every subscription starts with a snapshot, so resubscribing after a drop never misses the delivery.
        while True:
            state = core.StreamState(computer, None)
            async with aclosing(self._stream(core.message_events_path(computer, id), state, 1.0, once=True)) as s:
                async for ev in s:
                    turn = core.delivered_turn(ev, computer, id)
                    if turn is not None:
                        return turn
            await asyncio.sleep(1.0)

    async def _stream(
        self, path: str, state: core.StreamState, reconnect: float, *, once: bool = False
    ) -> AsyncGenerator[Event, None]:
        while True:
            try:
                async with self._http.stream("GET", self.url + path, headers=state.headers(self._token)) as res:
                    if not res.is_success:
                        await res.aread()
                        state.refused(res.status_code, res.text)
                    else:
                        # Only reading the body is retried: a read that fails is the connection dropping. A
                        # malformed frame or an exception thrown into the generator ends the stream instead.
                        decoder = SSEDecoder()
                        try:
                            async for chunk in res.aiter_bytes():
                                for frame in decoder.feed(chunk):
                                    ev = state.handle(frame)
                                    if ev is not None:
                                        yield ev
                        except httpx.TransportError:
                            pass
                        if once:
                            return
            except httpx.TransportError:
                pass
            await asyncio.sleep(reconnect)


_Item = Union[
    tuple[Literal["event"], Event],
    tuple[Literal["error"], BaseException],
    tuple[Literal["done", "wake"], None],
]
_Source = Callable[[str, Callable[[], None]], AsyncGenerator[Event, None]]


class AsyncEventStream:
    """Events of one or more Computers, merged in arrival order: one task per Computer feeds the iterator.

    ``async with client.events() as events:`` awaits ``ready()`` on entry and closes the stream on exit.
    """

    def __init__(self, source: _Source, names: list[str] | Callable[[], Awaitable[list[str]]]) -> None:
        self._source = source
        self._names = names
        self._queue: asyncio.Queue[_Item] = asyncio.Queue()
        self._tasks: set[asyncio.Task[None]] = set()
        self._started = False
        self._closed = False
        self._remaining = -1
        self._live = 0
        self._failed = False
        self._ready = asyncio.Event()
        self._ready_error: BaseException | None = None

    async def ready(self) -> None:
        """See EventStream.ready."""
        self._start()
        await self._ready.wait()
        if self._ready_error is not None:
            raise self._ready_error

    def close(self) -> None:
        self._closed = True
        for t in self._tasks:
            t.cancel()
        self._ready.set()
        self._queue.put_nowait(("wake", None))

    async def aclose(self) -> None:
        """close(), then waits for the streams' connections to be released."""
        self.close()
        await asyncio.gather(*self._tasks, return_exceptions=True)

    def __aiter__(self) -> AsyncEventStream:
        return self

    async def __anext__(self) -> Event:
        self._start()
        while True:
            if self._closed or self._remaining == 0:
                try:
                    item = self._queue.get_nowait()
                except asyncio.QueueEmpty:
                    raise StopAsyncIteration from None
            else:
                item = await self._queue.get()
            if item[0] == "event":
                return item[1]
            if item[0] == "error":
                self.close()
                raise item[1]
            if item[0] == "done":
                self._remaining -= 1

    async def __aenter__(self) -> AsyncEventStream:
        try:
            await self.ready()
        except BaseException:
            await self.aclose()
            raise
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        await self.aclose()

    def _start(self) -> None:
        if self._started:
            return
        self._started = True
        self._spawn(self._run())

    def _spawn(self, coro: Awaitable[None]) -> None:
        task: asyncio.Task[None] = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _run(self) -> None:
        try:
            names = await self._names() if callable(self._names) else self._names
        except Exception as err:
            self._remaining = 0
            self._fail(err)
            return
        self._remaining = len(names)
        if not names:
            self._ready.set()
        for name in names:
            self._spawn(self._pump(name, self._live_once(len(names))))
        self._queue.put_nowait(("wake", None))

    def _live_once(self, total: int) -> Callable[[], None]:
        counted = False

        def on_live() -> None:
            nonlocal counted
            if counted:
                return
            counted = True
            self._live += 1
            if self._live == total:
                self._ready.set()

        return on_live

    async def _pump(self, name: str, on_live: Callable[[], None]) -> None:
        try:
            async with aclosing(self._source(name, on_live)) as events:
                async for ev in events:
                    self._queue.put_nowait(("event", ev))
        except Exception as err:
            if not self._closed:
                self._fail(err)
        finally:
            on_live()
            self._queue.put_nowait(("done", None))

    def _fail(self, err: BaseException) -> None:
        if self._failed:
            return
        self._failed = True
        if not self._ready.is_set():
            self._ready_error = err
            self._ready.set()
        self._queue.put_nowait(("error", err))


class AsyncTurnStream:
    """Finished Turns of one or more Computers. Same ``ready()``, ``close()`` and ``async with`` as AsyncEventStream."""

    def __init__(self, events: AsyncEventStream) -> None:
        self._events = events
        self._builder = core.TurnBuilder()

    async def ready(self) -> None:
        await self._events.ready()

    def close(self) -> None:
        self._events.close()

    async def aclose(self) -> None:
        await self._events.aclose()

    def __aiter__(self) -> AsyncTurnStream:
        return self

    async def __anext__(self) -> Turn:
        async for ev in self._events:
            turn = self._builder.add(ev)
            if turn is not None:
                return turn
        raise StopAsyncIteration

    async def __aenter__(self) -> AsyncTurnStream:
        await self._events.__aenter__()
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        await self.aclose()
