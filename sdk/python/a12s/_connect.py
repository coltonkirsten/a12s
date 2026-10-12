"""connect() finds a running Fleet from the files `a12s up` writes."""

from __future__ import annotations

import json
import os
import re
from pathlib import Path

from ._async import AsyncClient
from ._client import Client

_FLOW = re.compile(r"""^metadata:\s*\{[^}]*\bname:\s*["']?([A-Za-z0-9-]+)""", re.M)
_BLOCK = re.compile(r"""^metadata:\s*(?:#.*)?\n(?:[ \t]+.*\n)*?[ \t]+name:\s*["']?([A-Za-z0-9-]+)""", re.M)


def connect(
    *,
    fleet: str | None = None,
    url: str | None = None,
    token: str | None = None,
    home: str | os.PathLike[str] | None = None,
    cwd: str | os.PathLike[str] | None = None,
) -> Client:
    """A client for a running Fleet, as its owner. It uses the first of:

    1. ``url`` (and ``token``), or ``$A12S_URL`` (and ``$A12S_TOKEN``);
    2. the Fleet named by ``fleet``;
    3. the Fleet file in ``$A12S_FLEET``, else the nearest ``fleet.yaml`` in ``cwd`` (default: the current directory)
       or a parent;
    4. the only Fleet with a running Controller on this machine.

    For 2-4 it reads ``<home>/.a12s/<fleet>/controller.json`` (default home: the home directory), which ``a12s up``
    writes.
    """
    return Client(*_discover(fleet, url, token, home, cwd))


async def connect_async(
    *,
    fleet: str | None = None,
    url: str | None = None,
    token: str | None = None,
    home: str | os.PathLike[str] | None = None,
    cwd: str | os.PathLike[str] | None = None,
) -> AsyncClient:
    """connect() for asyncio."""
    return AsyncClient(*_discover(fleet, url, token, home, cwd))


def _discover(
    fleet: str | None,
    url: str | None,
    token: str | None,
    home: str | os.PathLike[str] | None,
    cwd: str | os.PathLike[str] | None,
) -> tuple[str, str | None]:
    url = url if url is not None else os.environ.get("A12S_URL")
    if url:
        return url, token if token is not None else os.environ.get("A12S_TOKEN")
    root = Path(home if home is not None else Path.home()) / ".a12s"
    name = fleet or _fleet_name_nearby(Path(cwd if cwd is not None else os.getcwd())) or _only_running_fleet(root)
    path = root / name / "controller.json"
    try:
        cf = json.loads(path.read_text(encoding="utf-8"))
        return f"http://127.0.0.1:{int(cf['port'])}", cf.get("token")
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as err:
        raise RuntimeError(f"Fleet {name} isn't running (no {path}): run a12s up in its directory first") from err


def _fleet_name_nearby(cwd: Path) -> str | None:
    env = os.environ.get("A12S_FLEET")
    file = (cwd / env).resolve() if env else _find_up(cwd, "fleet.yaml")
    if file is None:
        return None
    name = _fleet_name(file.read_text(encoding="utf-8"))
    if not name:
        raise RuntimeError(f"{file} has no metadata.name")
    return name


def _find_up(start: Path, name: str) -> Path | None:
    for d in (start.resolve(), *start.resolve().parents):
        if (d / name).is_file():
            return d / name
    return None


def _fleet_name(yaml: str) -> str | None:
    """metadata.name from a Fleet file, in either ``metadata: { name: x }`` or block form."""
    m = _FLOW.search(yaml) or _BLOCK.search(yaml)
    return m.group(1) if m else None


def _only_running_fleet(root: Path) -> str:
    try:
        names = sorted(p.name for p in root.iterdir())
    except OSError:
        names = []  # no ~/.a12s yet
    running = [n for n in names if (root / n / "controller.json").is_file()]
    if len(running) == 1:
        return running[0]
    if not running:
        raise RuntimeError(
            "no running Fleet found: run a12s up in your Fleet's directory, or pass connect(url=..., token=...)"
        )
    raise RuntimeError(
        f"several Fleets are running ({', '.join(running)}): pass connect(fleet=\"<name>\") or run from the Fleet's "
        "directory"
    )
