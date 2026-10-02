//! Create a desktop, run JavaScript in it, put it to sleep.
//!
//!     COMPUTERUSE_API_TOKEN=bjs_... cargo run --example quickstart
//!
//! The token needs the scopes `sessions:write` and `sessions:connect`.

use computeruse::Client;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let token = std::env::var("COMPUTERUSE_API_TOKEN")
        .map_err(|_| "set COMPUTERUSE_API_TOKEN to an API token (bjs_...)")?;
    let mut builder = Client::builder().api_token(token);
    // For a deployment of your own: COMPUTERUSE_BASE_URL=https://api.example.com
    if let Ok(base_url) = std::env::var("COMPUTERUSE_BASE_URL") {
        builder = builder.base_url(base_url);
    }
    let client = builder.build()?;

    let session = client
        .sessions()
        .create()
        .name("quickstart")
        .policy_preset("unrestricted")
        .send()
        .await?;
    println!("created {} ({})", session.id(), session.mcp_url());

    // The call waits for the desktop to start.
    let result = session
        .run_js("console.log('6 * 7 =', 6 * 7)".into())
        .await?;
    print!("{}", result.output);
    if let Some(error) = result.error {
        eprintln!("the program failed: {error}");
    }

    let info = session.sleep().await?;
    println!("{} is {:?}", info.id, info.state);
    Ok(())
}
