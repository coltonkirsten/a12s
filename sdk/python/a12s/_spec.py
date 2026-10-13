"""The agent computer spec: the fleet file's agent computer as keyword arguments. Every field is optional; anything
left out comes from the Fleet's defaults (maps merge, lists replace, credentials add to the defaults'). Nested keys
are the JSON's, camelCase."""

from __future__ import annotations

from typing import Any, Literal, TypedDict, Union


class HarnessSpec(TypedDict, total=False):
    kind: Literal["claude-code", "codex"]
    version: str
    model: str


class ResourcesSpec(TypedDict, total=False):
    cpu: Union[str, float]
    memory: str


class EgressSpec(TypedDict, total=False):
    allow: list[str]


class SecuritySpec(TypedDict, total=False):
    seccomp: dict[str, Any]
    """A seccomp profile in Docker's JSON format, replacing the default for this agent computer."""


class InboxSpec(TypedDict, total=False):
    maxMessages: int
    maxBytes: str


WakeOnMessageSpec = TypedDict("WakeOnMessageSpec", {"from": list[str], "maxWakesPerHour": int}, total=False)


class PowerSpec(TypedDict, total=False):
    sleepAfterIdle: str
    wakeOnMessage: WakeOnMessageSpec


class UseSpec(TypedDict, total=False):
    name: str
    mcp: bool


class GrantsSpec(TypedDict, total=False):
    send: list[str]


class AgentComputerSpec(TypedDict, total=False):
    harness: HarnessSpec
    image: str
    instructions: str
    """Written as CLAUDE.md (claude-code) or AGENTS.md (codex) on the home drive."""
    files: dict[str, str]
    """More text files for the home drive, by path relative to it; up to 1 MiB with the instructions."""
    resources: ResourcesSpec
    credentials: list[str]
    egress: EgressSpec
    security: SecuritySpec
    inbox: InboxSpec
    power: PowerSpec
    uses: list[Union[str, UseSpec]]
    grants: GrantsSpec


class FleetSpec(dict[str, Any]):
    """The Fleet as written (GET /v1/fleet): a dict, with the top-level keys also as attributes.

    ``fleet.replace(lambda f: f.defaults.update(power={"sleepAfterIdle": "15m"}))`` edits it in place.
    """

    def _section(self, key: str) -> dict[str, Any]:
        v = self.get(key)
        if not isinstance(v, dict):
            v = self[key] = {}
        return v

    @property
    def defaults(self) -> dict[str, Any]:
        return self._section("defaults")

    @property
    def agent_computers(self) -> dict[str, Any]:
        return self._section("agent_computers")

    @property
    def services(self) -> dict[str, Any]:
        return self._section("services")

    @property
    def metadata(self) -> dict[str, Any]:
        return self._section("metadata")
