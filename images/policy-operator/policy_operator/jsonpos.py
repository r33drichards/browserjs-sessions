"""Rows and columns of the values of a JSON text, by path.

json.loads says where a syntax error is but not where a value was, and the
diagnostics of a JSON policy point into the text its author wrote.
"""
from __future__ import annotations

import bisect
import json
from json.decoder import scanstring

Path = tuple  # of object keys (str) and array indexes (int)
_WS = " \t\n\r"
_decoder = json.JSONDecoder()


def _skip(text: str, i: int) -> int:
    while i < len(text) and text[i] in _WS:
        i += 1
    return i


def _scan(text: str, i: int, path: Path, out: dict[Path, int]) -> int:
    i = _skip(text, i)
    out.setdefault(path, i)
    ch = text[i]
    if ch == "{":
        i = _skip(text, i + 1)
        if text[i] == "}":
            return i + 1
        while True:
            i = _skip(text, i)
            key, j = scanstring(text, i + 1)
            # An object's member is where its key is: that is what a reader
            # looks for when told "allow.rules[0].operation".
            out[path + (key,)] = i
            j = _skip(text, j)
            i = _skip(text, _scan(text, j + 1, path + (key,), out))
            if text[i] == ",":
                i += 1
                continue
            return i + 1
    if ch == "[":
        i = _skip(text, i + 1)
        if text[i] == "]":
            return i + 1
        n = 0
        while True:
            i = _skip(text, _scan(text, i, path + (n,), out))
            n += 1
            if text[i] == ",":
                i += 1
                continue
            return i + 1
    _, end = _decoder.raw_decode(text, i)
    return end


def positions(text: str) -> dict[Path, tuple[int, int]]:
    """Path to (row, col), both from 1, for every value of a valid JSON text.

    Empty when the text cannot be walked: positions are a help, never a
    reason to fail.
    """
    offsets: dict[Path, int] = {}
    try:
        _scan(text, 0, (), offsets)
    except (IndexError, ValueError, RecursionError):
        return {}
    starts = [0]
    for i, ch in enumerate(text):
        if ch == "\n":
            starts.append(i + 1)
    out = {}
    for path, off in offsets.items():
        row = bisect.bisect_right(starts, off)
        out[path] = (row, off - starts[row - 1] + 1)
    return out


def locate(pos: dict[Path, tuple[int, int]], path: Path) -> tuple[int, int] | None:
    """The position of `path`, or of the nearest enclosing value that has one."""
    path = tuple(path)
    while True:
        if path in pos:
            return pos[path]
        if not path:
            return None
        path = path[:-1]
