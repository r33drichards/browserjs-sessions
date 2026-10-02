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

# The generated bindings. maturin writes them, with the native library, next
# to this file when it builds the wheel.
from .computeruse import *  # noqa: F401,F403

__version__ = sdk_version()  # noqa: F405
