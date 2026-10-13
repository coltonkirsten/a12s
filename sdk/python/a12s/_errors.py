from __future__ import annotations

from typing import Any


class A12sError(Exception):
    """A refused call: the HTTP status and the Controller's error, including its fix line.

    ``code`` is stable: ``busy``, ``asleep``, ``no_grant``, ``inbox_full``, ``not_found`` and so on.
    """

    def __init__(
        self,
        status: int,
        code: str,
        message: str,
        fix: str | None = None,
        details: dict[str, str] | None = None,
    ) -> None:
        super().__init__(f"{code}: {message}  fix: {fix}" if fix else f"{code}: {message}")
        self.status = status
        self.code = code
        self.message = message
        self.fix = fix
        self.details = details


class NotFound(A12sError):
    """No agent computer, Service or other object by that name: ``get()`` on a missing name raises it."""


class A12sGapError(Exception):
    """Raised by an event stream with ``on_gap="throw"`` when events were lost."""

    def __init__(self, data: dict[str, Any]) -> None:
        after = data.get("after")
        super().__init__(
            f"journal gap after seq {after if after is not None else '(none)'}: events were lost; resync with status()"
        )
        self.after: int | None = after
        self.data = data


class A12sStreamError(Exception):
    """An event stream received a frame that is not a valid journal event."""


class A12sWarning(UserWarning):
    """A write that succeeded with a warning the Controller attached (``warnings`` in its answer)."""
