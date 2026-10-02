"""What the operator is told: read once from the environment (deploy.md,
"Workloads")."""
from __future__ import annotations

import os
import re
from dataclasses import dataclass, field

from .meter import MAX_GAP

MODES = ("off", "meter", "enforce")
_DURATION = re.compile(r"^(\d+)(ms|s|m|h)?$")
_UNIT = {"s": 1, "m": 60, "h": 3600}


class ConfigError(ValueError):
    pass


def seconds(value: str, name: str) -> int:
    """`60`, `60s`, `5m`, `1h`: whole seconds."""
    m = _DURATION.match(value.strip())
    if not m or m.group(2) == "ms":
        raise ConfigError(f"{name}={value!r} is not a number of seconds (60, 60s, 5m, 1h)")
    return int(m.group(1)) * _UNIT[m.group(2) or "s"]


@dataclass(frozen=True)
class Config:
    # off (or unset): the operator starts and does nothing. meter and enforce
    # are the same to the observer: it observes and sends usage.
    billing: str = "off"
    tick: int = 60
    max_gap: int = MAX_GAP
    awake_window: int = 300      # what is sent is added up over these (metronome.md)
    kept_window: int = 21600
    catalogue: str = "/etc/browserjs/catalogue.yaml"
    namespace: str = "browserjs-sessions"
    metronome_url: str = "https://api.metronome.com"
    # From the Secret `metronome`. Never logged; repr() does not show it.
    metronome_token: str = field(default="", repr=False)

    @property
    def metering(self) -> bool:
        return self.billing in ("meter", "enforce")

    @classmethod
    def from_env(cls, env=None) -> "Config":
        env = os.environ if env is None else env
        billing = (env.get("BILLING") or "off").strip().lower()
        if billing not in MODES:
            raise ConfigError(f"BILLING={billing!r} is not one of {', '.join(MODES)}")
        cfg = cls(
            billing=billing,
            tick=seconds(env.get("TICK") or "60s", "TICK"),
            max_gap=seconds(env.get("MAX_GAP") or f"{MAX_GAP}s", "MAX_GAP"),
            awake_window=seconds(env.get("AWAKE_WINDOW") or "5m", "AWAKE_WINDOW"),
            kept_window=seconds(env.get("KEPT_WINDOW") or "6h", "KEPT_WINDOW"),
            catalogue=env.get("BILLING_CATALOGUE") or cls.catalogue,
            namespace=env.get("BILLING_NAMESPACE") or cls.namespace,
            metronome_url=env.get("METRONOME_URL") or cls.metronome_url,
            metronome_token=(env.get("METRONOME_API_TOKEN") or "").strip(),
        )
        if cfg.metering and not cfg.metronome_token:
            raise ConfigError("METRONOME_API_TOKEN is not set (the Secret metronome): required when BILLING is not off")
        if cfg.tick < 1:
            raise ConfigError("TICK must be at least a second")
        for name, window in (("AWAKE_WINDOW", cfg.awake_window), ("KEPT_WINDOW", cfg.kept_window)):
            if window < cfg.tick:
                raise ConfigError(f"{name} ({window}s) must be at least TICK ({cfg.tick}s)")
        if cfg.max_gap <= cfg.tick:
            # Two sights a TICK apart would never be within MAX_GAP: nothing
            # would ever be charged, silently.
            raise ConfigError(f"MAX_GAP ({cfg.max_gap}s) must be longer than TICK ({cfg.tick}s)")
        return cfg
