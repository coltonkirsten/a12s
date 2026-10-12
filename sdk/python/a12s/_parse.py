from __future__ import annotations

import dataclasses
import functools
import types
import typing
from typing import Any, NamedTuple, TypeVar, Union

T = TypeVar("T")


class _Field(NamedTuple):
    name: str
    key: str
    hint: Any
    required: bool


def _camel(name: str) -> str:
    head, *rest = name.split("_")
    return head + "".join(part[:1].upper() + part[1:] for part in rest)


@functools.cache
def _fields(cls: type) -> tuple[_Field, ...]:
    hints = typing.get_type_hints(cls)
    return tuple(
        _Field(
            f.name,
            _camel(f.name),
            hints[f.name],
            f.default is dataclasses.MISSING and f.default_factory is dataclasses.MISSING,
        )
        for f in dataclasses.fields(cls)
    )


def from_json(cls: type[T], data: Any, *, strict: bool = False) -> T:
    """Builds a dataclass from the API's camelCase JSON.

    Unknown keys are ignored and a missing required field gets its type's zero value, as Go would send it, so a newer
    Controller never breaks an older client. ``strict`` turns both into errors and checks value types: the fixture
    tests use it to catch drift from the Go API types.
    """
    value: T = _value(cls, data, strict, cls.__name__)
    return value


def _value(hint: Any, data: Any, strict: bool, path: str) -> Any:
    if hint is Any:
        return data
    origin = typing.get_origin(hint)
    if origin is Union or origin is types.UnionType:
        args = [a for a in typing.get_args(hint) if a is not type(None)]
        if data is None:
            return None
        if len(args) != 1:
            raise TypeError(f"{path}: unsupported type {hint}")
        return _value(args[0], data, strict, path)
    if origin is list:
        if not isinstance(data, list):
            return _mismatch(data, "a list", strict, path, [])
        (item,) = typing.get_args(hint)
        return [_value(item, x, strict, f"{path}[{i}]") for i, x in enumerate(data)]
    if origin is dict:
        if not isinstance(data, dict):
            return _mismatch(data, "an object", strict, path, {})
        _, item = typing.get_args(hint)
        return {k: _value(item, v, strict, f"{path}.{k}") for k, v in data.items()}
    if dataclasses.is_dataclass(hint) and isinstance(hint, type):
        return _dataclass(hint, data, strict, path)
    if hint is bool:
        ok = isinstance(data, bool)
    elif hint is int:
        ok = isinstance(data, int) and not isinstance(data, bool)
    elif hint is float:
        ok = isinstance(data, (int, float)) and not isinstance(data, bool)
    elif hint is str:
        ok = isinstance(data, str)
    else:
        raise TypeError(f"{path}: unsupported type {hint}")
    if not ok and strict:
        raise ValueError(f"{path}: want {hint.__name__}, got {data!r}")
    return data


def _mismatch(data: Any, want: str, strict: bool, path: str, zero: Any) -> Any:
    if strict:
        raise ValueError(f"{path}: want {want}, got {data!r}")
    return zero if data is None else data


def _dataclass(cls: type, data: Any, strict: bool, path: str) -> Any:
    if not isinstance(data, dict):
        _mismatch(data, "an object", strict, path, None)
        data = {}
    fields = _fields(cls)
    if strict:
        known = {f.key for f in fields}
        unknown = sorted(k for k in data if k not in known)
        if unknown:
            raise ValueError(f"{path}: unknown keys {unknown}")
    kwargs: dict[str, Any] = {}
    for f in fields:
        if f.key in data:
            kwargs[f.name] = _value(f.hint, data[f.key], strict, f"{path}.{f.key}")
        elif f.required:
            if strict:
                raise ValueError(f"{path}: missing {f.key}")
            kwargs[f.name] = _zero(f.hint)
    return cls(**kwargs)


def _zero(hint: Any) -> Any:
    origin = typing.get_origin(hint)
    if hint is Any or origin is Union or origin is types.UnionType:
        return None
    if origin is list:
        return []
    if origin is dict:
        return {}
    if dataclasses.is_dataclass(hint) and isinstance(hint, type):
        return _dataclass(hint, {}, False, hint.__name__)
    return {bool: False, int: 0, float: 0.0, str: ""}[hint]
