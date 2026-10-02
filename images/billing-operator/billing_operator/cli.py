"""python -m billing_operator: the observer without a cluster or Metronome.

  simulate <dir> <time> [<time> ...]   the pass over a directory of Sandbox YAML, once per
                                       time; prints the seconds counted and the usage
                                       events sent at each tick (windows that closed)
  vectors <metering-vectors.json>      every vector of the contract through the seconds function
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from pathlib import Path

import yaml

from .catalogue import CatalogueFile
from .config import Config
from .memory import MemoryKube
from .meter import MAX_GAP, iso, seconds, ts
from .passes import Observer
from .sender import FakeMetronome


def default_catalogue() -> Path:
    """$BILLING_CATALOGUE; in a checkout (images/billing-operator/billing_operator/cli.py),
    the contract's; in the image, where the ConfigMap is mounted."""
    if os.environ.get("BILLING_CATALOGUE"):
        return Path(os.environ["BILLING_CATALOGUE"])
    parents = Path(__file__).resolve().parents
    contract = parents[3] / "docs" / "contracts" / "billing" / "catalogue.yaml" if len(parents) > 3 else None
    return contract if contract is not None and contract.is_file() else Path(Config.catalogue)


def read_sandboxes(directory: Path) -> list[dict]:
    sandboxes = []
    for path in sorted(p for p in directory.iterdir() if p.suffix in (".yaml", ".yml")):
        for doc in yaml.safe_load_all(path.read_text(encoding="utf-8")):
            docs = doc.get("items") or [] if isinstance(doc, dict) and doc.get("kind", "").endswith("List") else [doc]
            sandboxes += [d for d in docs if isinstance(d, dict) and d.get("kind") == "Sandbox"]
    return sandboxes


def run_vectors(path: Path, out) -> int:
    vectors = json.loads(path.read_text(encoding="utf-8"))
    bad = 0
    for v in vectors:
        sessions, got = (v.get("state") or {}).get("sessions") or {}, []
        for tick in v["ticks"]:
            sessions, counted = seconds(sessions, tick["observed"], tick["now"])
            got.append(counted.as_expect())
        want = [{k: e[k] for k in ("awakeSeconds", "diskGBSeconds")} for e in v["expect"]]
        if got != want:
            bad += 1
            print("FAIL", v["name"], file=out)
            for i, (g, e) in enumerate(zip(got, want)):
                if g != e:
                    print("  tick", i, "got", g, "want", e, file=out)
    print(f"{len(vectors) - bad}/{len(vectors)} vectors pass (awakeSeconds, diskGBSeconds)", file=out)
    return 1 if bad else 0


def selfcheck(out) -> int:
    """An hour awake, a tick a minute, is 3600 seconds and 3600 x 5 GB-seconds.
    Run when the image is built."""
    sessions = {"s": {"lastSeen": "2026-10-02T10:00:00Z", "awake": True}}
    awake = disk = 0
    for i in range(1, 61):
        sessions, counted = seconds(sessions, {"s": {"awake": True, "readySince": None, "diskGB": 5}},
                                    iso(ts("2026-10-02T10:00:00Z") + 60 * i))
        awake += sum(counted.awake.values())
        disk += sum(counted.disk_gb.values())
    ok = (awake, disk) == (3600, 18000)
    print(f"an hour awake: {awake} seconds, {disk} GB-seconds: {'ok' if ok else 'WRONG'}", file=out)
    return 0 if ok else 1


def simulate(directory: Path, times: list[str], catalogue: Path, max_gap: int, out,
             awake_window: int = 300, kept_window: int = 21600) -> int:
    sink = FakeMetronome()
    observer = Observer(MemoryKube(read_sandboxes(directory)), sink, CatalogueFile(catalogue), max_gap=max_gap,
                        holder="simulate", awake_window=awake_window, kept_window=kept_window)

    async def go() -> None:
        for when in times:
            before = len(sink.events)
            result = await observer.run_once(ts(when))
            yaml.safe_dump_all([{
                "tick": result.now, "pass": result.line(),
                "sessions": {sid: s for sid, s in sorted(observer.sessions.items())},
                "events": [e.body() for e in list(sink.events.values())[before:]],
            }], out, sort_keys=False, explicit_start=True, width=200)
    asyncio.run(go())
    return 0


def main(argv: list[str] | None = None, out=None) -> int:
    out = out or sys.stdout
    parser = argparse.ArgumentParser(prog="python -m billing_operator", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    s = sub.add_parser("simulate", help="run the pass over a directory of Sandbox YAML at each of the times")
    s.add_argument("directory", type=Path)
    s.add_argument("times", nargs="+", metavar="time", help="RFC 3339, e.g. 2026-10-02T10:00:00Z, in order")
    s.add_argument("--max-gap", type=int, default=MAX_GAP)
    s.add_argument("--awake-window", type=int, default=300, help="seconds (AWAKE_WINDOW)")
    s.add_argument("--kept-window", type=int, default=21600, help="seconds (KEPT_WINDOW)")
    s.add_argument("--catalogue", type=Path, default=None,
                   help="catalogue.yaml, for sessionDiskGB (default: $BILLING_CATALOGUE, or the contract's in a checkout)")
    v = sub.add_parser("vectors", help="run metering-vectors.json through the seconds function")
    v.add_argument("file", type=Path)
    sub.add_parser("selfcheck", help="an hour awake is 3600 seconds")
    args = parser.parse_args(argv)

    if args.command == "selfcheck":
        return selfcheck(out)

    if args.command == "vectors":
        return run_vectors(args.file, out)
    catalogue = args.catalogue or default_catalogue()
    if not catalogue.is_file():
        print(f"no catalogue at {catalogue}: pass --catalogue", file=sys.stderr)
        return 2
    return simulate(args.directory, args.times, catalogue, args.max_gap, out, args.awake_window, args.kept_window)
