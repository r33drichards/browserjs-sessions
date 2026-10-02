"""Where things are and who may call: read once from the environment."""
from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

GROUP = "browserjs.dev"
VERSION = "v1alpha1"
PLURAL = "sessionpolicies"

# A policy, as written and as Rego: rego-contract.md and the CRD.
MAX_SOURCE_BYTES = 65536
# operator-api.yaml.
MAX_VALIDATE_BODY = 128 * 1024
MAX_INPUT_BYTES = 1024 * 1024
MAX_EVALUATE_BODY = MAX_VALIDATE_BODY + MAX_INPUT_BYTES
EVAL_DEADLINE_SECONDS = 2
# status.errors and status.warnings: maxItems in the CRD.
MAX_DIAGNOSTICS = 50
# The longest a bundle request is held, whatever its Prefer header asks.
MAX_LONG_POLL_SECONDS = 300


def _contract_dir() -> Path:
    env = os.environ.get("POLICY_CONTRACT_DIR")
    if env:
        # Absolute: opa runs in directories of its own.
        return Path(env).resolve()
    image = Path("/contracts")
    if image.is_dir():
        return image
    # A checkout: images/policy-operator/policy_operator/config.py.
    return Path(__file__).resolve().parents[3] / "docs" / "contracts" / "policy"


@dataclass(frozen=True)
class Config:
    opa_bin: str = "opa"
    contract_dir: Path = Path("/contracts")
    namespace: str = "browserjs-sessions"
    opa_service: str = "opa"
    opa_port: int = 8181
    http_port: int = 8080
    # deploy.md: the three keys of the Secret policy-tokens.
    bundle_token: str = ""
    opa_token: str = ""
    api_token: str = ""

    @property
    def capabilities(self) -> Path:
        return self.contract_dir / "capabilities.json"

    @property
    def decision_template(self) -> Path:
        return self.contract_dir / "decision-module.rego.tmpl"

    @classmethod
    def from_env(cls) -> "Config":
        env = os.environ
        return cls(
            opa_bin=env.get("OPA_BIN", "opa"),
            contract_dir=_contract_dir(),
            namespace=env.get("POLICY_NAMESPACE", "browserjs-sessions"),
            opa_service=env.get("OPA_SERVICE", "opa"),
            opa_port=int(env.get("OPA_PORT", "8181")),
            http_port=int(env.get("HTTP_PORT", "8080")),
            bundle_token=env.get("BUNDLE_TOKEN", ""),
            opa_token=env.get("OPA_TOKEN", ""),
            api_token=env.get("OPERATOR_API_TOKEN", ""),
        )
