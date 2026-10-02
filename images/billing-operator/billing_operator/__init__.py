"""The browserjs billing operator: the observer that sends usage to Metronome.

`kopf run -m billing_operator` (deploy.md) imports this package, which
registers the handlers. `python -m billing_operator` is the command line.
"""
from . import handlers  # noqa: F401
