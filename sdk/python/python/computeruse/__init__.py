"""Client for Computer Use (https://computeruse.site).

Serverless, resumable desktop containers that an agent drives with one tool,
``run_js``. This package is the Rust SDK behind UniFFI bindings: every call
that reaches the network is a coroutine.

    import asyncio, os
    from computeruse import Client, CreateSessionRequest

    async def main():
        client = Client.with_token(os.environ["COMPUTERUSE_API_TOKEN"])
        session = await client.create_session(CreateSessionRequest(name="demo"))
        result = await session.run_js("console.log(6 * 7)")
        print(result.output)
        await session.sleep()

    asyncio.run(main())
"""

# The generated module (maturin writes it next to this file at build time).
from . import computeruse as _generated
from .computeruse import *  # noqa: F401,F403

__all__ = list(_generated.__all__)
__version__ = _generated.sdk_version()

del _generated
