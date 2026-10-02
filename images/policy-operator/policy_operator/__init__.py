"""The browserjs policy operator: SessionPolicy resources into one OPA bundle.

`kopf run -m policy_operator` (deploy.md) imports this package, which
registers the handlers. `python -m policy_operator` is the command line.
"""
from . import handlers  # noqa: F401
