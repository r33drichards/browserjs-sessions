"""The bundle: rego-contract.md, "The bundle"."""
from __future__ import annotations

import json
import re
import tempfile
from dataclasses import dataclass
from pathlib import Path

from . import opa
from .config import Config

@dataclass(frozen=True)
class Tenant:
    """What one session contributes: its rewritten module and its hash."""
    module: str
    hash: str


class BundleError(Exception):
    """`opa build` refused the directory. `errors` are OPA's, each with the
    session it is about when its location names one."""

    def __init__(self, errors: list[dict]):
        super().__init__("; ".join(str(e.get("message")) for e in errors) or "opa build failed")
        self.errors = errors

    def sessions(self) -> set[str]:
        return {e["session"] for e in self.errors if e.get("session")}


def write_tree(cfg: Config, root: Path, tenants: dict[str, Tenant]) -> None:
    template = cfg.decision_template.read_text(encoding="utf-8")
    (root / "browserjs" / "loaded").mkdir(parents=True)
    (root / "tenant").mkdir()
    (root / "decision").mkdir()
    (root / ".manifest").write_text('{"roots": ["browserjs"]}\n', encoding="utf-8")
    loaded = {sid: tenants[sid].hash for sid in sorted(tenants)}
    (root / "browserjs" / "loaded" / "data.json").write_text(json.dumps(loaded, sort_keys=True) + "\n", encoding="utf-8")
    for sid, tenant in tenants.items():
        (root / "tenant" / f"{sid}.rego").write_text(tenant.module, encoding="utf-8")
        (root / "decision" / f"{sid}.rego").write_text(template.replace("{{SESSION_ID}}", sid), encoding="utf-8")


def build(cfg: Config, tenants: dict[str, Tenant], revision: str) -> bytes:
    """browserjs.tar.gz for these sessions, or BundleError."""
    with tempfile.TemporaryDirectory(prefix="bundle-") as d:
        root = Path(d, "src")
        write_tree(cfg, root, tenants)
        # Built from inside the directory: OPA names each module in the
        # bundle by the path it was given, so this is what makes the names
        # tenant/<id>.rego, and the same sessions the same bytes.
        proc = opa.run(cfg.opa_bin, ["build", "-b", ".", "--capabilities", str(cfg.capabilities),
                                     "-r", revision, "-o", "../browserjs.tar.gz"], root, timeout=120)
        out = Path(d, "browserjs.tar.gz")
        if proc.returncode != 0 or not out.exists():
            raise BundleError(_build_errors(proc))
        return out.read_bytes()


_ERROR = re.compile(r"((?:tenant|decision)/(s-[a-z0-9]+)\.rego):(\d+): (\w+): (.*)")


def _build_errors(proc) -> list[dict]:
    # opa build has no JSON output: "N error(s) occurred: <file>:<row>: <code>: <message>", one a line.
    text = (proc.stderr or proc.stdout).decode("utf-8", "replace").strip()
    errors = [{"code": m.group(4), "message": f"{m.group(1)}:{m.group(3)}: {m.group(5).strip()}", "session": m.group(2)}
              for m in _ERROR.finditer(text)]
    return errors or [{"code": "rego_compile_error", "message": text or "opa build failed"}]
