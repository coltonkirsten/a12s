from __future__ import annotations

import queue
import threading
import time
import uuid
from collections.abc import Callable, Generator, Iterator, Sequence
from contextlib import closing, contextmanager
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


class _Cancel:
    """Stops stream threads: closing their responses unblocks a read that would otherwise wait for the next frame."""

    def __init__(self) -> None:
        self._event = threading.Event()
        self._lock = threading.Lock()
        self._responses: set[httpx.Response] = set()

    def set(self) -> None:
        self._event.set()
        with self._lock:
            responses = list(self._responses)
        for r in responses:
            try:
                r.close()
            except Exception:
                pass

    def is_set(self) -> bool:
        return self._event.is_set()

    def wait(self, seconds: float) -> None:
        self._event.wait(seconds)

    @contextmanager
    def track(self, res: httpx.Response) -> Iterator[None]:
        with self._lock:
            self._responses.add(res)
        try:
            yield
        finally:
            with self._lock:
                self._responses.discard(res)


class Client:
    """A client for the a12s Controller API. ``token`` is the owner token or a scoped one from ``a12s token create``.

    Pass ``http`` to use your own httpx.Client (proxies, transports); the Client then leaves closing it to you.
    """

    def __init__(self, url: str, token: str | None = None, *, http: httpx.Client | None = None) -> None:
        self.url = url.rstrip("/")
        self._token = token
        self._owns_http = http is None
        self._http = http if http is not None else httpx.Client(timeout=core.TIMEOUT)

    def close(self) -> None:
        if self._owns_http:
            self._http.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def _request(self, method: str, path: str, body: Any = None, headers: dict[str, str] | None = None) -> Any:
        res = self._http.request(method, self.url + path, **core.json_request(self._token, body, headers))
        return core.decode(res)

    def status(self) -> Status:
        """Every Computer and Service: phase, session, active Turn, inbox, power."""
        return from_json(Status, self._request("GET", "/v1/status"))

    def computer(self, name: str) -> ComputerStatus:
        for c in self.status().agent_computers:
            if c.name == name:
                return c
        raise A12sError(404, "not_found", f"no agent computer {name}")

    def send(
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
        """Puts a Message in a Computer's inbox and returns at once.

        It is delivered at the Computer's next idle moment, batched with anything else pending, and works while the
        Computer is busy or asleep. A retry with the same ``idempotency_key`` (random if unset) returns the original
        Message; ``expires`` is in seconds, 0 meaning now or never.
        """
        m = core.enqueue_body(body, sender, metadata, fresh, idempotency_key, expires, replace_key)
        return from_json(EnqueueResponse, self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/inbox", m))

    def prompt(
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
        """Sends a Message, waits for the Turn that delivers it and returns when that Turn ends.

        ``on_event`` gets every event of the Turn as it streams.
        """
        message = self.send(
            computer,
            body,
            sender=sender,
            metadata=metadata,
            fresh=fresh,
            idempotency_key=idempotency_key,
            expires=expires,
            replace_key=replace_key,
        )
        turn = self._delivered_in(computer, message.id)
        builder = core.TurnBuilder()
        # A gap mid-Turn would silently drop text: fail loudly instead.
        state = core.StreamState(computer, None, "throw")
        with closing(self._stream(core.events_path(computer, turn), state, 1.0, _Cancel())) as events:
            for ev in events:
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
    ) -> EventStream:
        """Live events of one Computer, several, or (None) every Computer in the Fleet, merged.

        Each event carries its ``computer``. A dropped stream reconnects after ``reconnect`` seconds without losing or
        repeating events. With one Computer, ``turn`` replays that Turn from its start (the stream ends after its
        turn.finished) and ``after`` resumes after a journal seq. When a stream can't resume from its cursor, ``on_gap="yield"`` yields a ``gap`` event and carries
        on live and ``"throw"`` raises A12sGapError; either way events were lost: resync from ``status()``.
        """
        names = core.stream_names(computers, turn, after)

        def source(name: str, live: Callable[[], None], cancel: _Cancel) -> Iterator[Event]:
            state = core.StreamState(name, after, on_gap, live)
            events = self._stream(core.events_path(name, turn), state, reconnect, cancel)
            return events if turn is None else core.until_turn_finished(events)

        return EventStream(source, names if names is not None else lambda: [c.name for c in self.status().agent_computers])

    def turns(
        self,
        computers: str | Sequence[str] | None = None,
        *,
        reconnect: float = 1.0,
        on_gap: core.OnGap = "yield",
    ) -> TurnStream:
        """Finished Turns of one Computer, several or all, as they end. Same ``ready()`` and ``close()`` as events()."""
        return TurnStream(self.events(computers, reconnect=reconnect, on_gap=on_gap))

    def inbox(
        self, computer: str, *, state: str | None = None, bodies: bool = False, after: int | None = None
    ) -> InboxList:
        """The inbox; ``state`` is a Message state or "all" (default: pending and claimed)."""
        return from_json(InboxList, self._request("GET", core.inbox_path(computer, state, bodies, after)))

    def message(self, computer: str, id: str) -> Message:
        return from_json(Message, self._request("GET", f"/v1/agent-computers/{core.enc(computer)}/inbox/{core.enc(id)}"))

    def withdraw(self, computer: str, id: str) -> Message:
        """Withdraws a pending Message; a 409 A12sError names its state if it is too late."""
        path = f"/v1/agent-computers/{core.enc(computer)}/inbox/{core.enc(id)}"
        return from_json(Message, self._request("DELETE", path))

    def stop(self, computer: str, turn: str | None = None) -> None:
        """Interrupts a Turn (default: the active one). The Turn ends ``interrupted``; nothing is replayed."""
        id = turn if turn is not None else self.computer(computer).active_turn
        if id:
            self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/turns/{core.enc(id)}/stop", {})

    def sleep(self, computer: str, *, hold: bool = False) -> None:
        """Stops the agent computer's processes and keeps its disks, session and inbox. A Message from a sender its
        power policy lists (wakeOnMessage) wakes it; with ``hold`` it stays asleep until wake()."""
        body: dict[str, Any] = {"state": "asleep"}
        if hold:
            body["hold"] = True
        self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/power", body)

    def wake(self, computer: str, *, clear_inbox: bool = False) -> None:
        """Starts a sleeping agent computer; its session resumes on the next Turn. ``clear_inbox`` discards the
        pending Messages first."""
        body: dict[str, Any] = {"state": "awake"}
        if clear_inbox:
            body["clearInbox"] = True
            body["requestId"] = uuid.uuid4().hex
        self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/power", body)

    def hold(self, computer: str, *, reason: str | None = None) -> InboxList:
        """Holds the agent computer's inbox: Messages still queue, none is delivered until resume()."""
        body = {"reason": reason} if reason else {}
        return from_json(InboxList, self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/inbox/hold", body))

    def resume(self, computer: str) -> InboxList:
        """Releases a hold; the pending Messages are delivered at the next idle moment."""
        return from_json(InboxList, self._request("POST", f"/v1/agent-computers/{core.enc(computer)}/inbox/resume", {}))

    def services(self) -> list[ServiceStatus]:
        res = self._request("GET", "/v1/services")
        return [from_json(ServiceStatus, s) for s in (res or {}).get("services") or []]

    def service(self, name: str) -> ServiceStatus:
        return from_json(ServiceStatus, self._request("GET", f"/v1/services/{core.enc(name)}"))

    def create_token(
        self,
        name: str,
        actions: Sequence[str] | None = None,
        computers: Sequence[str] | None = None,
        *,
        prefix: str | None = None,
    ) -> Token:
        """A token for another program; owner only. The secret is in the response once."""
        req: dict[str, Any] = {"name": name}
        if actions is not None:
            req["actions"] = list(actions)
        if computers is not None:
            req["computers"] = list(computers)
        if prefix is not None:
            req["prefix"] = prefix
        return from_json(Token, self._request("POST", "/v1/tokens", req))

    def put_fleet(self, request: dict[str, Any]) -> dict[str, Any]:
        """What ``a12s up`` sends: validate and apply a whole Fleet. Most apps leave this to the CLI."""
        res: dict[str, Any] = self._request("PUT", "/v1/fleet", request)
        return res

    def _delivered_in(self, computer: str, id: str) -> str:
        # Every subscription starts with a snapshot, so resubscribing after a drop never misses the delivery.
        while True:
            state = core.StreamState(computer, None)
            with closing(self._stream(core.message_events_path(computer, id), state, 1.0, _Cancel(), once=True)) as s:
                for ev in s:
                    turn = core.delivered_turn(ev, computer, id)
                    if turn is not None:
                        return turn
            time.sleep(1.0)

    def _stream(
        self, path: str, state: core.StreamState, reconnect: float, cancel: _Cancel, *, once: bool = False
    ) -> Generator[Event, None, None]:
        while not cancel.is_set():
            try:
                with self._http.stream("GET", self.url + path, headers=state.headers(self._token)) as res:
                    with cancel.track(res):
                        if not res.is_success:
                            res.read()
                            state.refused(res.status_code, res.text)
                        else:
                            # Only reading the body is retried: a read that fails is the connection dropping. A
                            # malformed frame or an exception thrown into the generator ends the stream instead.
                            decoder = SSEDecoder()
                            try:
                                for chunk in res.iter_bytes():
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
            cancel.wait(reconnect)


_Item = Union[
    tuple[Literal["event"], Event],
    tuple[Literal["error"], BaseException],
    tuple[Literal["done", "wake"], None],
]
_Source = Callable[[str, Callable[[], None], _Cancel], Iterator[Event]]


class EventStream:
    """Events of one or more Computers, merged in arrival order: one thread per Computer feeds the iterator.

    ``with client.events() as events:`` waits for ``ready()`` on entry and closes the stream on exit.
    """

    def __init__(self, source: _Source, names: list[str] | Callable[[], list[str]]) -> None:
        self._source = source
        self._names = names
        self._cancel = _Cancel()
        self._queue: queue.SimpleQueue[_Item] = queue.SimpleQueue()
        self._lock = threading.Lock()
        self._started = False
        self._remaining = -1
        self._live = 0
        self._failed = False
        self._ready = threading.Event()
        self._ready_error: BaseException | None = None

    def ready(self) -> None:
        """Returns once every stream is connected and live; events that happen after this are never missed.

        A sleeping Computer counts as live: its events start when it wakes. Raises if a stream fails first.
        """
        self._start()
        self._ready.wait()
        if self._ready_error is not None:
            raise self._ready_error

    def close(self) -> None:
        self._cancel.set()
        self._ready.set()
        self._queue.put(("wake", None))

    def __iter__(self) -> EventStream:
        return self

    def __next__(self) -> Event:
        self._start()
        while True:
            try:
                item = self._queue.get(block=not self._cancel.is_set() and self._remaining != 0)
            except queue.Empty:
                raise StopIteration from None
            if item[0] == "event":
                return item[1]
            if item[0] == "error":
                self.close()
                raise item[1]
            if item[0] == "done":
                self._remaining -= 1

    def __enter__(self) -> EventStream:
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

    def _start(self) -> None:
        with self._lock:
            if self._started:
                return
            self._started = True
        try:
            names = self._names() if callable(self._names) else self._names
        except Exception as err:
            self._remaining = 0
            self._fail(err)
            return
        self._remaining = len(names)
        if not names:
            self._ready.set()
        for name in names:
            on_live = self._live_once(len(names))
            threading.Thread(target=self._pump, args=(name, on_live), name=f"a12s-events-{name}", daemon=True).start()

    def _live_once(self, total: int) -> Callable[[], None]:
        counted = False

        def on_live() -> None:
            nonlocal counted
            with self._lock:
                if counted:
                    return
                counted = True
                self._live += 1
                if self._live == total:
                    self._ready.set()

        return on_live

    def _pump(self, name: str, on_live: Callable[[], None]) -> None:
        try:
            for ev in self._source(name, on_live, self._cancel):
                self._queue.put(("event", ev))
        except Exception as err:
            if not self._cancel.is_set():
                self._fail(err)
        finally:
            on_live()
            self._queue.put(("done", None))

    def _fail(self, err: BaseException) -> None:
        with self._lock:
            if self._failed:
                return
            self._failed = True
            if not self._ready.is_set():
                self._ready_error = err
                self._ready.set()
        self._queue.put(("error", err))


class TurnStream:
    """Finished Turns of one or more Computers. Same ``ready()``, ``close()`` and ``with`` as EventStream."""

    def __init__(self, events: EventStream) -> None:
        self._events = events
        self._builder = core.TurnBuilder()

    def ready(self) -> None:
        self._events.ready()

    def close(self) -> None:
        self._events.close()

    def __iter__(self) -> TurnStream:
        return self

    def __next__(self) -> Turn:
        for ev in self._events:
            turn = self._builder.add(ev)
            if turn is not None:
                return turn
        raise StopIteration

    def __enter__(self) -> TurnStream:
        self._events.__enter__()
        return self

    def __exit__(
        self, exc_type: type[BaseException] | None, exc: BaseException | None, tb: TracebackType | None
    ) -> None:
        self.close()
