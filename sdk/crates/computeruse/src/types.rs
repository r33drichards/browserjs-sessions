use crate::BuildError;
use computeruse_sdk_macros::UniffiBuilder;
use serde::{Deserialize, Serialize};
use std::fmt;

/// The API host of the hosted service.
pub const DEFAULT_BASE_URL: &str = "https://api.computeruse.site";

/// Scope: list and read sessions.
pub const SCOPE_SESSIONS_READ: &str = "sessions:read";
/// Scope: create, rename, stop, resume, sleep, wake and delete sessions.
pub const SCOPE_SESSIONS_WRITE: &str = "sessions:write";
/// Scope: call a session's MCP endpoint, which is what `run_js` needs.
pub const SCOPE_SESSIONS_CONNECT: &str = "sessions:connect";
/// Scope: read a session's policy.
pub const SCOPE_POLICIES_READ: &str = "policies:read";
/// Scope: write a session's policy and its management mode.
pub const SCOPE_POLICIES_WRITE: &str = "policies:write";

/// How a [`Client`](crate::Client) is made. Only `api_token` is required.
///
/// Its `Debug` does not print the token.
#[derive(Clone, PartialEq, Eq, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(BuildError)]
pub struct ClientOptions {
    /// An API token (`bjs_<id>_<secret>`), or an access token made from one.
    pub api_token: String,
    /// The API host, without a path. Default: `https://api.computeruse.site`.
    /// `http` is accepted for a loopback address only.
    #[uniffi(default = None)]
    pub base_url: Option<String>,
    /// Narrows the access token to these scopes (a subset of the API
    /// token's). Default: all of the API token's.
    #[uniffi(default = None)]
    pub scopes: Option<Vec<String>>,
    /// Exchange the API token for a short-lived access token and send that
    /// (the default), or send the API token itself on every request.
    #[uniffi(default = None)]
    pub exchange_token: Option<bool>,
    /// The time limit of one API request, in milliseconds. Default 60 000.
    #[uniffi(default = None)]
    pub timeout_ms: Option<u64>,
    /// The time limit of one MCP request, in milliseconds. Default 330 000:
    /// `run_js` may run for 300 seconds.
    #[uniffi(default = None)]
    pub mcp_timeout_ms: Option<u64>,
    /// How many times a request is tried again after `429`, `502`, `503`,
    /// `504` or a connection that could not be made. Default 3.
    #[uniffi(default = None)]
    pub max_retries: Option<u32>,
    /// The first pause between tries, in milliseconds; it doubles each
    /// time. Default 500. A `Retry-After` the API sends is used instead.
    #[uniffi(default = None)]
    pub retry_base_delay_ms: Option<u64>,
    /// How long an MCP call keeps trying while the session is waking
    /// (`504` with `Retry-After`), in milliseconds. Default 180 000.
    #[uniffi(default = None)]
    pub wake_timeout_ms: Option<u64>,
    /// Added in front of the SDK's own `User-Agent`.
    #[uniffi(default = None)]
    pub user_agent: Option<String>,
}

impl fmt::Debug for ClientOptions {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ClientOptions")
            .field("api_token", &"<redacted>")
            .field("base_url", &self.base_url)
            .field("scopes", &self.scopes)
            .field("exchange_token", &self.exchange_token)
            .field("timeout_ms", &self.timeout_ms)
            .field("mcp_timeout_ms", &self.mcp_timeout_ms)
            .field("max_retries", &self.max_retries)
            .field("retry_base_delay_ms", &self.retry_base_delay_ms)
            .field("wake_timeout_ms", &self.wake_timeout_ms)
            .field("user_agent", &self.user_agent)
            .finish()
    }
}

/// Who a token acts as: `GET /v1/me`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Me {
    pub email: String,
    #[serde(default)]
    pub name: String,
    /// Always false with a token: a token is never an admin.
    #[serde(default)]
    pub admin: bool,
}

/// A session's lifecycle state.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "lowercase")]
pub enum SessionState {
    /// The desktop is starting or waking.
    Starting,
    /// The desktop is up.
    Running,
    /// The desktop is shutting down: a stop, a sleep or a delete.
    Stopping,
    /// No compute; disk and snapshot kept. An MCP call wakes it.
    Asleep,
    /// Stopped on request; disk kept. An MCP call answers 409.
    Stopped,
    /// The desktop could not start; `message` says why.
    Failed,
    /// A state this version of the SDK does not know.
    #[serde(other)]
    Unknown,
}

/// A session as the API shows it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct SessionInfo {
    /// `s-` and five or ten characters.
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub owner: String,
    pub state: SessionState,
    /// Why it is starting or failed.
    #[serde(default)]
    pub message: Option<String>,
    /// RFC 3339.
    #[serde(default)]
    pub created: Option<String>,
    /// The MCP URL the API reports, which is the one people sign in to. A
    /// token uses [`Session::mcp_url`](crate::Session::mcp_url) instead.
    #[serde(default)]
    pub mcp_url: Option<String>,
    /// Absent where policies are off.
    #[serde(default)]
    pub policy: Option<PolicySummary>,
    /// True when a suspended session holds a snapshot of its running
    /// desktop, which a wake restores. Absent otherwise.
    #[serde(default, rename = "stateSaved")]
    pub state_saved: Option<bool>,
    /// Why an asleep or stopped session is so (`user`, `sleep`, `idle`,
    /// `credit`, `payment-method`, `blocked`). Only where billing is on.
    #[serde(default, rename = "stoppedBy")]
    pub stopped_by: Option<String>,
    /// The same reasons, while it finishes its calls before such a sleep.
    #[serde(default)]
    pub draining: Option<String>,
    /// RFC 3339: when it will be deleted for its account being at zero.
    #[serde(default, rename = "deleteAfter")]
    pub delete_after: Option<String>,
}

/// Whether a policy is in force.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "lowercase")]
pub enum PolicyState {
    /// In force on every replica.
    Ready,
    /// Saved; not yet loaded everywhere.
    Loading,
    /// Does not compile; the previous policy, if any, is still in force.
    Invalid,
    /// The session predates policies and cannot have one.
    Unsupported,
    /// A state this version of the SDK does not know.
    #[serde(other)]
    Unknown,
}

/// Who manages a policy.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "lowercase")]
pub enum ManagementMode {
    /// The editor in the app. A token may not write it, unless the write
    /// moves it to `Iac`.
    Editor,
    /// Code: Terraform, or this SDK. The app may not write it.
    Iac,
    /// A mode this version of the SDK does not know.
    #[serde(other)]
    Unknown,
}

/// Who manages a policy, and where its source lives.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(BuildError)]
pub struct Management {
    pub mode: ManagementMode,
    /// Required, and https, when `mode` is `Iac`: where the policy's source
    /// is kept. The app links to it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[uniffi(default = None)]
    pub managed_url: Option<String>,
}

/// What a session carries about its policy.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct PolicySummary {
    /// `rego`.
    #[serde(default)]
    pub kind: Option<String>,
    #[serde(default)]
    pub version: Option<i64>,
    /// Of the policy in force.
    #[serde(default)]
    pub hash: Option<String>,
    pub state: PolicyState,
    #[serde(default)]
    pub management: Option<Management>,
}

/// One error or warning about a policy's source. `row` and `col` are
/// 1-based.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Diagnostic {
    #[serde(default)]
    pub row: Option<u32>,
    #[serde(default)]
    pub col: Option<u32>,
    #[serde(default)]
    pub code: String,
    #[serde(default)]
    pub message: String,
}

/// How many policy-engine replicas have the policy.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Loaded {
    #[serde(default)]
    pub replicas: u32,
    #[serde(default)]
    pub total: u32,
}

/// A session's policy, with its source.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Policy {
    #[serde(default)]
    pub kind: Option<String>,
    #[serde(default)]
    pub version: Option<i64>,
    #[serde(default)]
    pub hash: Option<String>,
    pub state: PolicyState,
    #[serde(default)]
    pub management: Option<Management>,
    /// What was written.
    #[serde(default)]
    pub source: Option<String>,
    /// The module in force: the last source that compiled.
    #[serde(default)]
    pub rego: Option<String>,
    #[serde(default)]
    pub errors: Vec<Diagnostic>,
    #[serde(default)]
    pub warnings: Vec<Diagnostic>,
    #[serde(default)]
    pub loaded: Option<Loaded>,
    /// RFC 3339.
    #[serde(default)]
    pub updated: Option<String>,
    /// `ui` or `token:<token name>`.
    #[serde(default)]
    pub updated_by: Option<String>,
}

/// A policy to write: Rego source, and optionally who manages it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(BuildError)]
pub struct PolicyInput {
    /// The Rego module.
    pub source: String,
    /// A token may write a policy in `editor` mode only by moving it to
    /// `iac` here.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[uniffi(default = None)]
    pub management: Option<Management>,
    /// Write only if the policy is still at this version (`If-Match`).
    #[serde(skip)]
    #[uniffi(default = None)]
    pub if_match_version: Option<i64>,
}

/// The verdict on a policy that was checked and not saved.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Validation {
    pub ok: bool,
    #[serde(default)]
    pub rego: Option<String>,
    #[serde(default)]
    pub hash: Option<String>,
    #[serde(default)]
    pub errors: Vec<Diagnostic>,
    #[serde(default)]
    pub warnings: Vec<Diagnostic>,
}

/// What a policy decides about one sample call.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Evaluation {
    /// Whether the policy could be evaluated at all.
    pub ok: bool,
    /// The decision, when `ok`.
    #[serde(default)]
    pub allow: Option<bool>,
    #[serde(default)]
    pub errors: Vec<Diagnostic>,
}

/// A ready-made policy.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct PolicyPreset {
    /// For example `unrestricted` or `no-scripting`.
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub source: String,
}

/// A session to create. Every field is optional.
#[derive(Debug, Clone, Default, PartialEq, Eq, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(BuildError)]
pub struct CreateSessionRequest {
    /// 1 to 63 characters. Left out, the service picks one like
    /// `brave-otter`.
    #[uniffi(default = None)]
    pub name: Option<String>,
    /// The session's policy. Left out, with no preset: unrestricted.
    #[uniffi(default = None)]
    pub policy: Option<PolicyInput>,
    /// The id of a preset (`GET /v1/policy-presets`) to use as the policy.
    /// The SDK reads the preset and sends its source. Not with `policy`.
    #[uniffi(default = None)]
    pub policy_preset: Option<String>,
}

/// A `run_js` call.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(BuildError)]
pub struct RunJsRequest {
    /// JavaScript or TypeScript. Top-level `await` works.
    pub code: String,
    /// Heap limit in MB. Minimum 4, default 8.
    #[uniffi(default = None)]
    pub heap_memory_max_mb: Option<u32>,
    /// Time limit in seconds, 1 to 300. Default 30.
    #[uniffi(default = None)]
    pub execution_timeout_secs: Option<u32>,
}

/// One block of a tool's result.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct ContentBlock {
    /// `text`, `image`, `audio`, `resource`, ...
    pub kind: String,
    /// For `text`.
    pub text: Option<String>,
    /// For `image` and `audio`: the decoded bytes.
    pub data: Option<Vec<u8>>,
    pub mime_type: Option<String>,
    /// The block as the server sent it.
    pub raw_json: String,
}

/// What a tool answered.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct ToolResult {
    pub content: Vec<ContentBlock>,
    /// `structuredContent`, as JSON, when the tool sent one.
    pub structured_json: Option<String>,
}

impl ToolResult {
    /// The text blocks, joined by newlines.
    pub fn text(&self) -> String {
        self.content
            .iter()
            .filter_map(|block| block.text.as_deref())
            .collect::<Vec<_>>()
            .join("\n")
    }
}

/// What `run_js` answered.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct RunJsResult {
    /// What the code wrote with `console.log` and its siblings.
    pub output: String,
    /// Set when the code threw or ran out of time. The call itself
    /// succeeded: this is the program's failure, not the SDK's.
    pub error: Option<String>,
    /// What the code attached with `artifact(key, mime, bytes)`.
    pub artifacts: Vec<ContentBlock>,
    /// The tool's whole JSON answer.
    pub raw_json: String,
}

/// A tool of the session's MCP server.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct ToolInfo {
    pub name: String,
    pub description: Option<String>,
    /// The JSON Schema of its arguments.
    pub input_schema_json: String,
}
