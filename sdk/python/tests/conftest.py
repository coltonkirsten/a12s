from __future__ import annotations

import json
import socket
import threading
from collections.abc import Callable, Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from a12s._async import AsyncClient
from a12s._client import Client


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


class Req:
    def __init__(self, method: str, url: str, headers: dict[str, str], body: str) -> None:
        self.method = method
        self.url = url
        self.headers = headers
        self.body = body


class Res:
    """A Node-style response: chunked, so destroy() mid-body is a dropped connection, not a clean end."""

    def __init__(self, handler: BaseHTTPRequestHandler) -> None:
        self._h = handler
        self.ended = False

    def write_head(self, status: int, content_type: str = "text/event-stream") -> Res:
        self._h.send_response(status)
        self._h.send_header("content-type", content_type)
        self._h.send_header("transfer-encoding", "chunked")
        self._h.end_headers()
        return self

    def write(self, s: str) -> None:
        b = s.encode()
        self._h.wfile.write(f"{len(b):x}\r\n".encode() + b + b"\r\n")

    def end(self, s: str = "") -> None:
        if s:
            self.write(s)
        self._h.wfile.write(b"0\r\n\r\n")
        self.ended = True

    def json(self, status: int, body: Any) -> None:
        self.write_head(status, "application/json").end(json.dumps(body))

    def destroy(self) -> None:
        self._h.connection.shutdown(socket.SHUT_RDWR)
        self._h.close_connection = True
        self.ended = True


Handler = Callable[[Req, Res], None]


class FakeServer:
    """A real HTTP server in a thread, for the streaming and drop cases MockTransport can't model."""

    def __init__(self, handler: Handler) -> None:
        self.errors: list[BaseException] = []
        self._stop = threading.Event()
        self._conns: list[socket.socket] = []
        self._clients: list[Client] = []
        fake = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args: Any) -> None:
                pass

            def setup(self) -> None:
                super().setup()
                fake._conns.append(self.connection)

            def _serve(self) -> None:
                n = int(self.headers.get("content-length") or 0)
                body = self.rfile.read(n).decode() if n else ""
                req = Req(self.command, self.path, {k.lower(): v for k, v in self.headers.items()}, body)
                res = Res(self)
                try:
                    handler(req, res)
                except OSError:
                    res.ended = True
                except Exception as err:  # an assertion in the handler fails the test at teardown
                    fake.errors.append(err)
                    self.close_connection = True
                    return
                if not res.ended:
                    fake._stop.wait()
                    self.close_connection = True

            do_GET = do_POST = do_PUT = do_DELETE = _serve

        class Server(ThreadingHTTPServer):
            daemon_threads = True

            def handle_error(self, request: Any, client_address: Any) -> None:
                pass  # clients closing kept-alive connections

        self._srv = Server(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self._srv.server_address[1]}/"
        threading.Thread(target=self._srv.serve_forever, args=(0.05,), daemon=True).start()

    def client(self) -> Client:
        c = Client(self.url, "tok")
        self._clients.append(c)
        return c

    def aclient(self) -> AsyncClient:
        return AsyncClient(self.url, "tok")

    def close(self) -> None:
        for client in self._clients:
            client.close()
        self._stop.set()
        for c in self._conns:
            try:
                c.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self._srv.shutdown()
        self._srv.server_close()


@pytest.fixture
def fake() -> Iterator[Callable[[Handler], FakeServer]]:
    servers: list[FakeServer] = []

    def start(handler: Handler) -> FakeServer:
        s = FakeServer(handler)
        servers.append(s)
        return s

    yield start
    for s in servers:
        s.close()
    for s in servers:
        if s.errors:
            raise s.errors[0]


def ev(seq: int, type: str, turn: str, data: Any, computer: str = "lead") -> str:
    e = {"v": 1, "agentComputer": computer, "session": "s", "turn": turn, "seq": seq, "ts": "", "type": type, "data": data}
    return f"id: {seq}\ndata: {json.dumps(e)}\n\n"
