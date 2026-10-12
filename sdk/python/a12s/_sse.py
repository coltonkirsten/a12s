from __future__ import annotations

import codecs
import re
from typing import NamedTuple

_NEWLINE = re.compile(r"\r\n|\r|\n")


class SSEFrame(NamedTuple):
    id: str | None
    event: str | None
    data: str


class SSEDecoder:
    """Parses a text/event-stream body, fed in chunks, into frames. Comments (keepalives) are skipped."""

    def __init__(self) -> None:
        self._text = codecs.getincrementaldecoder("utf-8")(errors="replace")
        self._buf = ""
        self._id: str | None = None
        self._event: str | None = None
        self._data: list[str] = []

    def feed(self, chunk: bytes) -> list[SSEFrame]:
        self._buf += self._text.decode(chunk)
        frames: list[SSEFrame] = []
        while m := _NEWLINE.search(self._buf):
            if m.group() == "\r" and m.end() == len(self._buf):
                break  # a CRLF split across chunks
            line = self._buf[: m.start()]
            self._buf = self._buf[m.end() :]
            if line == "":
                if self._data or self._event:
                    frames.append(SSEFrame(self._id, self._event, "\n".join(self._data)))
                self._id, self._event, self._data = None, None, []
                continue
            if line.startswith(":"):
                continue
            field, colon, value = line.partition(":")
            if colon and value.startswith(" "):
                value = value[1:]
            if field == "data":
                self._data.append(value)
            elif field == "id":
                self._id = value
            elif field == "event":
                self._event = value
        return frames
