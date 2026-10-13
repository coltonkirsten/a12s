"""What the sync and async clients share: requests, the event stream cursor, Turn folding."""

from __future__ import annotations

import json
import warnings
import uuid
from collections.abc import AsyncGenerator, AsyncIterator, Callable, Iterator, Sequence
from dataclasses import dataclass, field, fields
from datetime import datetime, timezone
from typing import Any, Literal
from urllib.parse import quote, urlencode

import httpx

from ._errors import A12sError, A12sGapError, A12sStreamError, A12sWarning, NotFound
from ._parse import from_json
from ._sse import SSEFrame
from ._types import EnqueueResponse, Event, Reply, TokenUsage, ToolCall, Turn, TurnInboxMessage

OnGap = Literal["yield", "throw"]

# No overall timeout, like fetch: applying a Fleet can take minutes and event streams stay open indefinitely.
TIMEOUT = httpx.Timeout(None, connect=10.0)

_UNDELIVERED = ("withdrawn", "discarded", "expired", "replaced")


def enc(s: str) -> str:
    return quote(s, safe="!~*'()")


def headers(token: str | None, extra: dict[str, str] | None = None) -> dict[str, str]:
    h = dict(extra or {})
    if token:
        h["authorization"] = f"Bearer {token}"
    return h


def json_request(token: str | None, body: Any, extra: dict[str, str] | None = None) -> dict[str, Any]:
    if body is None:
        return {"headers": headers(token, extra)}
    return {"headers": headers(token, {**(extra or {}), "content-type": "application/json"}), "content": json.dumps(body)}


def decode(res: httpx.Response) -> Any:
    text = res.text
    if not res.is_success:
        raise api_error(res.status_code, text)
    return json.loads(text) if text else None


def api_error(status: int, text: str) -> A12sError:
    try:
        b = json.loads(text)
    except ValueError:
        b = None
    if isinstance(b, dict) and isinstance(b.get("code"), str):
        cls = NotFound if status == 404 and b["code"] == "not_found" else A12sError
        return cls(status, b["code"], str(b.get("message", "")), b.get("fix"), b.get("details"))
    return A12sError(status, f"http_{status}", text.strip() or f"HTTP {status}")


def enqueue_body(
    body: str,
    sender: str | None,
    metadata: dict[str, str] | None,
    fresh: bool,
    idempotency_key: str | None,
    expires: int | None,
    replace_key: str | None,
) -> dict[str, Any]:
    m: dict[str, Any] = {"body": body}
    if sender is not None:
        m["sender"] = sender
    if metadata is not None:
        m["metadata"] = metadata
    if fresh:
        m["fresh"] = True
    m["idempotencyKey"] = idempotency_key if idempotency_key is not None else str(uuid.uuid4())
    if expires is not None:
        m["expires"] = expires
    if replace_key is not None:
        m["replaceKey"] = replace_key
    return m


def inbox_path(computer: str, state: str | None, bodies: bool, after: int | None) -> str:
    q: dict[str, str] = {}
    if state:
        q["state"] = state
    if bodies:
        q["bodies"] = "true"
    if after is not None:
        q["after"] = str(after)
    qs = urlencode(q)
    return f"/v1/agent-computers/{enc(computer)}/inbox" + (f"?{qs}" if qs else "")


def stream_names(computers: str | Sequence[str] | None, turn: str | None, after: int | None) -> list[str] | None:
    names = [computers] if isinstance(computers, str) else None if computers is None else list(computers)
    if (turn is not None or after is not None) and (names is None or len(names) != 1):
        raise TypeError("events: turn and after need exactly one agent computer")
    return names


def events_path(computer: str, turn: str | None = None) -> str:
    q = f"?turn={enc(turn)}" if turn else ""
    return f"/v1/agent-computers/{enc(computer)}/events{q}"


def message_events_path(computer: str, id: str) -> str:
    return f"/v1/agent-computers/{enc(computer)}/inbox/{enc(id)}/events"


class StreamState:
    """One Computer's event stream across reconnects: the cursor and what each SSE frame means."""

    def __init__(
        self,
        computer: str,
        after: int | None,
        on_gap: OnGap = "yield",
        live: Callable[[], None] | None = None,
    ) -> None:
        self.computer = computer
        self.cursor = after
        self.on_gap = on_gap
        self._live = live

    def headers(self, token: str | None) -> dict[str, str]:
        extra = {"accept": "text/event-stream"}
        if self.cursor is not None:
            extra["last-event-id"] = str(self.cursor)
        return headers(token, extra)

    def refused(self, status: int, text: str) -> None:
        err = api_error(status, text)
        # Asleep or starting: the journal comes back with the Computer.
        if status not in (502, 503):
            raise err
        self.live()

    def live(self) -> None:
        if self._live is not None:
            self._live()

    def handle(self, f: SSEFrame) -> Event | None:
        if f.event == "head":
            # Everything up to the head has been sent: a reconnect resumes from there even if no event came.
            seq = _parse_json(f.data)
            seq = seq.get("seq") if isinstance(seq, dict) else None
            if isinstance(seq, (int, float)) and not isinstance(seq, bool):
                self.cursor = max(self.cursor or 0, int(seq))
            self.live()
            return None
        if f.event == "gap":
            server = _parse_json(f.data)
            data: dict[str, Any] = dict(server) if isinstance(server, dict) else {}
            data.pop("after", None)
            if self.cursor is not None:
                data["after"] = self.cursor
            self.cursor = None  # the stream goes live; a reconnect starts live too
            if self.on_gap == "throw":
                raise A12sGapError(data)
            ts = datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")
            return Event(
                v=1, agent_computer=self.computer, session="", turn="", seq=0, ts=ts, type="gap", data=data
            )
        if f.event or not f.data:
            return None
        try:
            raw = json.loads(f.data)
            if not isinstance(raw, dict):
                raise ValueError("not an object")
            ev = from_json(Event, raw)
            if f.id is not None:
                self.cursor = int(f.id)
        except ValueError as err:
            after = self.cursor if self.cursor is not None else "(start)"
            raise A12sStreamError(f"malformed event after seq {after}: {f.data[:120]}") from err
        return ev


def delivered_turn(ev: Event, computer: str, id: str) -> str | None:
    """The Turn that delivered the Message, None while it waits; raises if it can no longer be delivered."""
    d = ev.data if isinstance(ev.data, dict) else {}
    if ev.type == "snapshot":
        m = d.get("message")
        m = m if isinstance(m, dict) else {}
        state, turn = m.get("state"), m.get("turn")
    else:
        state, turn = ev.type, d.get("turn")
    if state == "delivered" and turn:
        return str(turn)
    if state in _UNDELIVERED:
        raise A12sError(409, str(state), f"message {id} was {state} before delivery")
    if (ev.type == "snapshot" and d.get("held")) or ev.type == "held":
        reason = d.get("holdReason") if d.get("holdReason") is not None else d.get("reason")
        raise A12sError(
            409,
            "held",
            f"{computer}'s inbox is held ({reason}); {id} waits in the inbox",
            fix=f"a12s inbox resume {computer}",
        )
    return None


@dataclass
class _OpenTurn:
    text: str = ""
    after_tool: bool = False
    messages: list[TurnInboxMessage] = field(default_factory=list)
    tools: dict[str, ToolCall] = field(default_factory=dict)
    started_at: str | None = None


class TurnBuilder:
    """Folds the events of one or more Computers into finished Turns."""

    def __init__(self) -> None:
        self._open: dict[tuple[str, str], _OpenTurn] = {}

    def add(self, ev: Event) -> Turn | None:
        if not ev.turn:
            return None
        key = (ev.agent_computer or "", ev.turn)
        t = self._open.setdefault(key, _OpenTurn())
        data = ev.data if isinstance(ev.data, dict) else {}
        if ev.type == "turn.started":
            inbox = data.get("inbox")
            messages = inbox.get("messages") if isinstance(inbox, dict) else None
            t.messages = [from_json(TurnInboxMessage, m) for m in messages] if isinstance(messages, list) else []
            t.started_at = ev.ts or None
        elif ev.type == "tool.started":
            t.after_tool = True
            tid = str(data.get("id", len(t.tools)))
            t.tools[tid] = ToolCall(id=tid, name=str(data.get("name", "")), input=data.get("input"))
        elif ev.type == "tool.finished":
            tid = str(data.get("id", ""))
            started = t.tools.get(tid)
            t.tools[tid] = ToolCall(
                id=tid,
                name=str(data.get("name", started.name if started else "")),
                input=started.input if started else None,
                output=data.get("output"),
                is_error=bool(data.get("isError")),
            )
        elif ev.type == "text.delta":
            text = data.get("text")
            if isinstance(text, str):
                # Text before and after a tool call are separate messages: keep them on separate lines.
                if t.after_tool and t.text and not t.text.endswith("\n"):
                    t.text += "\n"
                t.after_tool = False
                t.text += text
        elif ev.type == "turn.finished":
            del self._open[key]
            tokens = data.get("tokens")
            return Turn(
                agent_computer=ev.agent_computer or "",
                id=ev.turn,
                session=ev.session,
                status=str(data.get("status", "")),
                text=t.text,
                error=data.get("error") or None,
                messages=t.messages,
                tools=list(t.tools.values()),
                usage=data.get("usage"),
                tokens=from_json(TokenUsage, tokens) if isinstance(tokens, dict) else None,
                started_at=t.started_at,
                finished_at=ev.ts,
            )
        return None


def reply(turn: Turn, message: EnqueueResponse) -> Reply:
    return Reply(**{f.name: getattr(turn, f.name) for f in fields(turn)}, message=message)


def _parse_json(text: str) -> Any:
    try:
        return json.loads(text) if text else None
    except ValueError:
        return text


def turn_of(data: Any) -> Turn:
    """A Turn from the history (GET .../turns): the same shape turns() yields."""
    return from_json(Turn, data)


def until_turn_finished(events: Iterator[Event]) -> Iterator[Event]:
    """A Turn replay: its events up to and including its turn.finished. The server keeps the stream open after."""
    try:
        for ev in events:
            yield ev
            if ev.type == "turn.finished":
                return
    finally:
        close = getattr(events, "close", None)
        if close is not None:
            close()


async def until_turn_finished_async(events: AsyncIterator[Event]) -> AsyncGenerator[Event, None]:
    """until_turn_finished for asyncio."""
    try:
        async for ev in events:
            yield ev
            if ev.type == "turn.finished":
                return
    finally:
        aclose = getattr(events, "aclose", None)
        if aclose is not None:
            await aclose()


def warn_writes(res: Any) -> None:
    """A write's warnings (a grant to an agent computer that doesn't exist yet, a missing harness credential, a credential
    with no value yet) as A12sWarning, so they reach the developer instead of being dropped."""
    for w in (res.get("warnings") if isinstance(res, dict) else None) or []:
        warnings.warn(str(w), A12sWarning, stacklevel=3)
