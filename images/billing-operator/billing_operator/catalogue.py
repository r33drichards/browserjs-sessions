"""The catalogue (docs/contracts/billing/catalogue.yaml), as far as the
observer uses it: how many GB a session's disk is. The rates are
Metronome's (its rate card is made from the same file by the backend's
setup command).

It is a mounted ConfigMap. It is read again at every tick and taken when
its bytes changed; a file that does not parse keeps the last good one.
"""
from __future__ import annotations

import hashlib
import logging
from dataclasses import dataclass
from pathlib import Path

import yaml

log = logging.getLogger("billing_operator")


class CatalogueError(ValueError):
    pass


@dataclass(frozen=True)
class Catalogue:
    session_disk_gb: int


def parse(text: str) -> Catalogue:
    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as e:
        raise CatalogueError(f"not YAML: {e}") from e
    if not isinstance(doc, dict):
        raise CatalogueError("not a mapping")
    if doc.get("version") != 1:
        raise CatalogueError(f"version {doc.get('version')!r} is not 1")
    gb = doc.get("sessionDiskGB")
    if isinstance(gb, bool) or not isinstance(gb, int) or gb < 1:
        raise CatalogueError(f"sessionDiskGB must be a whole number of at least 1, not {gb!r}")
    return Catalogue(session_disk_gb=gb)


class CatalogueFile:
    """The catalogue at a path, re-read when it changes."""

    def __init__(self, path: str | Path):
        self.path = Path(path)
        self._good: Catalogue | None = None
        self._seen: str | None = None  # digest of the bytes last looked at, good or bad

    def current(self) -> Catalogue | None:
        """The catalogue in force: the file's, or the last good one when the
        file is missing or does not parse. None when there never was one."""
        try:
            raw = self.path.read_bytes()
        except OSError as e:
            if self._seen != "unreadable":
                log.error("catalogue %s cannot be read (%s); %s", self.path, e,
                          "keeping the last good one" if self._good else "there is none")
                self._seen = "unreadable"
            return self._good
        digest = hashlib.sha256(raw).hexdigest()
        if digest == self._seen:
            return self._good
        self._seen = digest
        try:
            parsed = parse(raw.decode("utf-8"))
        except (CatalogueError, UnicodeDecodeError) as e:
            log.error("catalogue %s does not parse (%s); %s", self.path, e,
                      "keeping the last good one" if self._good else "there is none")
            return self._good
        if self._good is not None and parsed != self._good:
            log.info("catalogue %s changed: %d GB a session, from this tick", self.path, parsed.session_disk_gb)
        self._good = parsed
        return self._good
