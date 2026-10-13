"""The JSON types of the a12s Controller API, with snake_case fields. They mirror internal/api: the JSON is the
contract."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True, slots=True, kw_only=True)
class Failure:
    reason: str
    retryable: bool


@dataclass(frozen=True, slots=True, kw_only=True)
class Harness:
    kind: str
    version: str
    model: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class LimitValue:
    value: int
    source: str


@dataclass(frozen=True, slots=True, kw_only=True)
class InboxLimits:
    max_messages: LimitValue
    max_bytes: LimitValue


@dataclass(frozen=True, slots=True, kw_only=True)
class InboxUsage:
    messages: int
    bytes: int


@dataclass(frozen=True, slots=True, kw_only=True)
class InboxEnded:
    """Messages that ended without delivery in the last 24 hours."""

    expired: int
    replaced: int
    withdrawn: int
    discarded: int


@dataclass(frozen=True, slots=True, kw_only=True)
class InboxSummary:
    computer: str
    held: bool
    hold_reason: str | None = None
    diagnostic: str | None = None
    pending: int
    claimed: int
    limits: InboxLimits
    usage: InboxUsage
    over_limit: bool
    ended24h: InboxEnded
    orphaned: bool | None = None
    session_override: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class PowerStatus:
    sleep_after_idle: str | None = None
    wake_on_message: list[str] | None = None
    max_wakes_per_hour: int | None = None
    auto_wake: str
    reason: str | None = None
    fix: str | None = None
    wakes_last_hour: int
    next_wake_at: str | None = None
    idle_since: str | None = None
    slept_by: str | None = None
    slept_at: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class ComputerStatus:
    name: str
    phase: str
    error: Failure | None = None
    resume_pending: bool | None = None
    session: str | None = None
    active_turn: str | None = None
    harness: Harness
    image: str
    seccomp: str | None = None
    autonomy: str
    generation: int
    staged_generation: int | None = None
    credential_generation: int | None = None
    home_size: str | None = None
    drives: list[str] | None = None
    egress_mode: str | None = None
    warnings: list[str] | None = None
    inbox: InboxSummary | None = None
    inbox_cleared: int | None = None
    power: PowerStatus | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class DriveRow:
    name: str
    kind: str
    computer: str | None = None
    service: str | None = None
    size: str | None = None
    access: str | None = None
    attached_to: list[str] | None = None
    orphaned: bool | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class ServiceStatus:
    name: str
    phase: str
    error: Failure | None = None
    image: str
    pin: str | None = None
    generation: int
    restarts: int
    health: str | None = None
    health_since: str | None = None
    send: list[str] | None = None
    uses: list[str] | None = None
    used_by: list[str] | None = None
    publish: list[str] | None = None
    ports: list[str] | None = None
    state_drive: str | None = None
    egress_mode: str | None = None
    warnings: list[str] | None = None
    spec: dict[str, Any] | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class Status:
    fleet: str
    egress_enforcement: str
    agent_computers: list[ComputerStatus]
    computers: list[ComputerStatus]  # agent_computers under its old name, for one release
    orphaned_drives: list[DriveRow] | None = None
    credentials_loaded: bool
    warnings: list[str] | None = None
    orphaned_inboxes: list[InboxSummary] | None = None
    services: list[ServiceStatus] | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class EnqueueResponse:
    id: str
    seq: int
    created_at: str
    expires_at: str | None
    state: str
    position: int
    phase: str | None = None
    replaced: str | None = None
    batch: str | None = None
    turn: str | None = None
    outcome: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class Message:
    id: str
    computer: str
    seq: int
    sender: str
    via: str
    created_at: str
    body: str | None = None
    metadata: dict[str, str] | None = None
    fresh: bool
    expires_at: str | None
    replace_key: str | None = None
    superseded: int
    replaced_by: str | None = None
    state: str
    batch: str | None = None
    turn: str | None = None
    attempts: int
    bytes: int
    ended_at: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class InboxList(InboxSummary):
    messages: list[Message]


@dataclass(frozen=True, slots=True, kw_only=True)
class Token:
    name: str
    actions: list[str]
    computers: list[str]
    prefix: str | None = None
    created_at: str
    secret: str | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class Event:
    """One event of a Computer's journal. ``data`` is the event's JSON as sent, camelCase keys included.

    ``seq`` is the Computer's journal position; ``turn`` is "" for events outside a Turn. A ``gap`` event is made by
    the SDK, never journaled: the stream could not resume from its cursor and events were lost.
    """

    v: int
    computer: str
    session: str
    turn: str
    seq: int
    ts: str
    type: str
    data: Any = None


@dataclass(frozen=True, slots=True, kw_only=True)
class TurnInboxMessage:
    """One Message a Turn carries. ``via`` is who really sent it: ``local``, ``token:<name>``, ``service:<name>``..."""

    id: str
    seq: int
    sender: str
    via: str
    created_at: str
    bytes: int
    key: str | None = None
    superseded: int | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class ToolCall:
    """One tool call in a Turn. ``send`` to another Computer is ``mcp__a12s__send`` with ``input["body"]``."""

    id: str
    name: str
    input: Any = None
    output: Any = None
    is_error: bool = False


@dataclass(frozen=True, slots=True, kw_only=True)
class Turn:
    """A finished Turn: who sent the work (``messages``), what the agent said (``text``) and how it ended.

    ``messages`` is empty for a continuation, or when the stream joined mid-Turn.
    """

    computer: str
    id: str
    session: str
    status: str
    text: str
    error: str | None = None
    messages: list[TurnInboxMessage]
    tools: list[ToolCall]
    usage: Any
    started_at: str | None = None
    finished_at: str


@dataclass(frozen=True, slots=True, kw_only=True)
class PromptResult(Turn):
    """The finished Turn and the Message that started it."""

    message: EnqueueResponse


@dataclass(frozen=True, slots=True, kw_only=True)
class Change:
    """One entry of a write's change list. ``when`` is "now", or "idle" when a busy agent computer applies it later."""

    op: str
    kind: str
    name: str
    when: str
    fields: list[str] | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class AgentComputerInfo:
    """GET /v1/agent-computers/{name}: the spec as written, with the defaults merged in, and the status."""

    name: str
    spec: dict[str, Any]
    effective: dict[str, Any]
    status: ComputerStatus


@dataclass(frozen=True, slots=True, kw_only=True)
class AgentComputersResponse:
    agent_computers: list[AgentComputerInfo]


@dataclass(frozen=True, slots=True, kw_only=True)
class FleetResponse:
    version: int
    fleet: dict[str, Any]
    drift: list[Change]


@dataclass(frozen=True, slots=True, kw_only=True)
class CredentialInfo:
    """A credential's name, whether it has a value, and who uses it. Values are never read back."""

    name: str
    set: bool
    used_by: list[str] | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class CredentialsResponse:
    credentials: list[CredentialInfo]
