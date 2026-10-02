//! Client for [Computer Use](https://computeruse.site): serverless,
//! resumable desktop containers that an agent drives with one tool,
//! `run_js`.
//!
//! The client speaks the API for code on `https://api.computeruse.site`
//! with an API token (`bjs_...`, made on the **API tokens** page of the
//! app): sessions and their lifecycle, session policies, and each
//! session's MCP endpoint.
//!
//! ```no_run
//! use computeruse::Client;
//!
//! # async fn example() -> Result<(), computeruse::ComputerUseError> {
//! let client = Client::builder()
//!     .api_token(std::env::var("COMPUTERUSE_API_TOKEN").unwrap())
//!     .build()?;
//!
//! // Create a desktop, run JavaScript in it, put it to sleep.
//! let session = client.sessions().create().name("demo").send().await?;
//! let result = session.run_js("console.log(6 * 7)".into()).await?;
//! println!("{}", result.output);
//! session.sleep().await?;
//! # Ok(())
//! # }
//! ```
//!
//! # What the client does for you
//!
//! - **Token exchange.** The API token is exchanged for an access token of
//!   an hour (`POST /oauth/token`, OAuth client credentials), which is
//!   what requests carry. It is replaced a minute before it expires, and
//!   at once after a `401`. Turn it off with
//!   [`ClientBuilder::exchange_token`] to send the API token itself.
//! - **Retries.** `429`, `502`, `503`, `504` and failed connections are
//!   tried again with a doubling pause, or the pause `Retry-After` asks
//!   for. A create is tried again only where the request was certainly
//!   not carried out. An MCP call to a session that is still waking is
//!   tried again until the wake timeout.
//! - **Typed errors.** [`ComputerUseError`] has a variant for each answer
//!   the API documents: `401`, `402`, `403`, `404`, `409`, `422`, `429`.
//! - **No secrets in `Debug`.** Neither the client, its options nor its
//!   builders print the token.
//!
//! # Scopes
//!
//! | Call | Scope |
//! |---|---|
//! | [`Client::me`], policy presets, validate, evaluate | any |
//! | list, get, [`Session::refresh`] | `sessions:read` |
//! | create, rename, stop, resume, sleep, wake, delete | `sessions:write` |
//! | [`Session::run_js`], [`Session::call_tool`] | `sessions:connect` |
//! | [`Session::policy`] | `policies:read` |
//! | [`Session::put_policy`] and its siblings | `policies:write` |
//!
//! # Other languages
//!
//! The same surface is exported through [UniFFI](https://mozilla.github.io/uniffi-rs/)
//! to Python (`pip install computeruse`), JavaScript (`npm install
//! computeruse`) and Go. The exported methods take owned records, each
//! with a generated builder ([`CreateSessionRequestBuilder`] and so on);
//! in Rust the borrowing builders of [`Client::builder`] and
//! [`Client::sessions`] read better.
//!
//! # Not here
//!
//! Billing endpoints (they are switched off in the service), API token
//! management (cookie only, by design), file transfer and the live view
//! (the app's API only).

mod client;
mod error;
mod fluent;
mod mcp;
mod session;
mod transport;
mod types;

pub use client::Client;
pub use error::{BuildError, ComputerUseError, MAX_ERROR_BODY_BYTES};
pub use fluent::{ClientBuilder, CreateSession, Sessions};
pub use session::Session;
pub use types::{
    ClientOptions, ClientOptionsBuilder, ContentBlock, CreateSessionRequest,
    CreateSessionRequestBuilder, Diagnostic, Evaluation, Loaded, Management, ManagementBuilder,
    ManagementMode, Me, Policy, PolicyInput, PolicyInputBuilder, PolicyPreset, PolicyState,
    PolicySummary, RunJsRequest, RunJsRequestBuilder, RunJsResult, SessionInfo, SessionState,
    ToolInfo, ToolResult, Validation, DEFAULT_BASE_URL, SCOPE_POLICIES_READ, SCOPE_POLICIES_WRITE,
    SCOPE_SESSIONS_CONNECT, SCOPE_SESSIONS_READ, SCOPE_SESSIONS_WRITE,
};

/// The SDK's version.
#[uniffi::export]
pub fn sdk_version() -> String {
    env!("CARGO_PKG_VERSION").to_owned()
}

uniffi::setup_scaffolding!("computeruse");
