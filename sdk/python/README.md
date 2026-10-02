# computeruse

Python client for [Computer Use](https://computeruse.site): serverless,
resumable desktop containers that an agent drives with one tool, `run_js`.

```bash
pip install computeruse
```

```python
import asyncio, os
from computeruse import Client, CreateSessionRequest

async def main():
    client = Client.with_token(os.environ["COMPUTERUSE_API_TOKEN"])

    # Create a desktop, run JavaScript in it, put it to sleep.
    session = await client.create_session(CreateSessionRequest(name="demo"))
    result = await session.run_js("console.log(6 * 7)")
    print(result.output)
    await session.sleep()

asyncio.run(main())
```

The token is an API token (`bjs_...`) from the **API tokens** page of the
app. Every call that reaches the network is a coroutine. Errors are
subclasses of `ComputerUseError`: `ComputerUseError.NotFound`,
`ComputerUseError.Conflict`, `ComputerUseError.PaymentRequired` and so on.

```python
from computeruse import ClientOptionsBuilder, ComputerUseError

options = (ClientOptionsBuilder()
    .api_token(token)
    .scopes(["sessions:read", "sessions:connect"])
    .timeout_ms(30_000)
    .build())
client = Client(options)

try:
    session = await client.get_session("s-abcde")
except ComputerUseError.NotFound:
    ...
```

This is the Rust SDK behind [UniFFI](https://mozilla.github.io/uniffi-rs/)
bindings: the wheel carries a native library and has no Python dependencies.
It is typed (`py.typed`). Wheels are built for Linux x86_64 and aarch64
(glibc 2.35 and newer) and macOS arm64 and x86_64, for Python 3.9 and newer.

Reference: <https://computeruse.site/reference/sdk>. Licence: Apache-2.0.
