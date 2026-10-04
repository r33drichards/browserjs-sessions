"""Container-only bootstrap: provide SessionPolicy resources without Kubernetes.

Capture, reconciliation, bundle publication, filtering, Redis persistence, and
HTTP delivery all use the production operator. The resource file is test input.
"""
import asyncio
import json
from pathlib import Path
from policy_operator.config import Config
from policy_operator.operator import Operator
from policy_operator.server import start, stop

async def main():
    path = Path('/var/lib/webhook-collector/resource.json')
    op = Operator(Config.from_env())
    doc = json.loads(path.read_text())
    await op.first_pass([doc])
    runner = await start(op)
    previous = path.read_text()
    try:
        while True:
            await asyncio.sleep(.1)
            current = path.read_text()
            if current != previous:
                doc = json.loads(current)
                await op.reconcile(doc['metadata']['name'], doc['spec'], {})
                previous = current
    finally:
        await stop(runner, op)

asyncio.run(main())
