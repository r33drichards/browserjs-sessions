# computeruse-sdk

Rust client for [Computer Use](https://computeruse.site): serverless,
resumable desktop containers that an agent drives with one tool, `run_js`.

```toml
[dependencies]
computeruse-sdk = "0.1"
tokio = { version = "1", features = ["macros", "rt-multi-thread"] }
```

The library is `computeruse` in code:

```rust,no_run
use computeruse::Client;

#[tokio::main]
async fn main() -> Result<(), computeruse::ComputerUseError> {
    let client = Client::builder()
        .api_token(std::env::var("COMPUTERUSE_API_TOKEN").unwrap())
        .build()?;

    // Create a desktop, run JavaScript in it, put it to sleep.
    let session = client.sessions().create().name("demo").send().await?;
    let result = session.run_js("console.log(6 * 7)".into()).await?;
    println!("{}", result.output);
    session.sleep().await?;
    Ok(())
}
```

An API token (`bjs_...`) is made on the **API tokens** page of the app. The
client exchanges it for a short-lived access token, refreshes that, retries
`429` and `5xx` with backoff, waits for a session that is waking, and maps
each documented answer to a variant of `ComputerUseError`. TLS is rustls
with the platform's trust roots; there is no OpenSSL.

The same client is published for Python (`computeruse` on PyPI), JavaScript
(`computeruse` on npm) and Go through UniFFI; see the
[SDK reference](https://computeruse.site/reference/sdk).

Licence: Apache-2.0.
