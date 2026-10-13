"""Python SDK for a12s: hold a Fleet, create agent computers on it and send them work.

    fleet = a12s.Fleet()
    hello = fleet.agent_computers.create("hello", instructions="Be helpful")
    hello.prompt("hi")
    hello.remove()
"""

from ._async import AsyncClient, AsyncEventStream, AsyncTurnStream
from ._client import Client, EventStream, TurnStream
from ._connect import connect, connect_async
from ._afleet import AsyncAgentComputer, AsyncAgentComputers, AsyncFleet, AsyncService, AsyncServices, AsyncTokens
from ._errors import A12sError, A12sGapError, A12sStreamError, NotFound
from ._fleet import AgentComputer, AgentComputers, Fleet, FleetState, Service, Services, Tokens
from ._spec import AgentComputerSpec, FleetSpec
from ._types import (
    AgentComputerInfo,
    Change,
    ComputerStatus,
    DriveRow,
    EnqueueResponse,
    Event,
    Failure,
    Harness,
    InboxEnded,
    InboxLimits,
    InboxList,
    InboxSummary,
    InboxUsage,
    LimitValue,
    Message,
    PowerStatus,
    PromptResult,
    ServiceStatus,
    Status,
    Token,
    ToolCall,
    Turn,
    TurnInboxMessage,
)

__all__ = [
    "A12sError",
    "A12sGapError",
    "A12sStreamError",
    "AgentComputer",
    "AgentComputerInfo",
    "AgentComputerSpec",
    "AgentComputers",
    "AsyncAgentComputer",
    "AsyncAgentComputers",
    "AsyncClient",
    "AsyncEventStream",
    "AsyncFleet",
    "AsyncService",
    "AsyncServices",
    "AsyncTokens",
    "AsyncTurnStream",
    "Change",
    "Client",
    "ComputerStatus",
    "DriveRow",
    "EnqueueResponse",
    "Event",
    "EventStream",
    "Failure",
    "Fleet",
    "FleetSpec",
    "FleetState",
    "Harness",
    "InboxEnded",
    "InboxLimits",
    "InboxList",
    "InboxSummary",
    "InboxUsage",
    "LimitValue",
    "Message",
    "NotFound",
    "PowerStatus",
    "PromptResult",
    "Service",
    "ServiceStatus",
    "Services",
    "Status",
    "Token",
    "Tokens",
    "ToolCall",
    "Turn",
    "TurnInboxMessage",
    "TurnStream",
    "connect",
    "connect_async",
]
