use crate::types::Diagnostic;
use thiserror::Error;

/// How much of an answer's body an error keeps.
pub const MAX_ERROR_BODY_BYTES: usize = 4_096;

/// Everything a call can fail with.
///
/// The HTTP answers the API documents each have a variant; any other status
/// is [`ComputerUseError::Api`]. No variant carries a token.
#[derive(Debug, Clone, PartialEq, Error, uniffi::Error)]
pub enum ComputerUseError {
    /// The client was configured with something it cannot use.
    #[error("invalid configuration: {reason}")]
    Configuration { reason: String },

    /// The request did not get an answer: DNS, TLS, a refused or dropped
    /// connection.
    #[error("transport error: {reason}")]
    Transport { reason: String },

    /// No answer within the client's timeout, or a wait that ran out.
    #[error("timed out: {operation}")]
    Timeout { operation: String },

    /// 401: the token is missing, malformed, unknown, revoked or expired, or
    /// its owner may no longer use the service.
    #[error("unauthorized (401): {message}")]
    Unauthorized { message: String },

    /// 402: billing refused the request. `code` says what for and
    /// `billing_url` is where the account's owner puts it right.
    #[error("payment required (402): {message}")]
    PaymentRequired {
        message: String,
        code: Option<String>,
        billing_url: Option<String>,
    },

    /// 403: the token lacks the scope the route needs, or is bound to
    /// another session; or billing has blocked the account (`code` and
    /// `billing_url` are then set).
    #[error("forbidden (403): {message}")]
    Forbidden {
        message: String,
        code: Option<String>,
        billing_url: Option<String>,
    },

    /// 404: no such session, or not the caller's. The API answers both
    /// alike.
    #[error("not found (404): {message}")]
    NotFound { message: String },

    /// 409: the session limit is reached, the session is stopped, or the
    /// policy is managed elsewhere (`managed_url` then says where). A limit
    /// of the account's plan carries `code` and `billing_url`.
    #[error("conflict (409): {message}")]
    Conflict {
        message: String,
        code: Option<String>,
        managed_url: Option<String>,
        billing_url: Option<String>,
    },

    /// 422: the policy does not validate. Nothing was saved.
    #[error("invalid policy (422): {message}")]
    InvalidPolicy {
        message: String,
        errors: Vec<Diagnostic>,
        warnings: Vec<Diagnostic>,
    },

    /// 429, after the client's own retries.
    #[error("rate limited (429): {message}")]
    RateLimited {
        message: String,
        retry_after_secs: Option<u64>,
    },

    /// Any other status that is not a success.
    #[error("the API answered {status}: {message}")]
    Api {
        status: u16,
        message: String,
        code: Option<String>,
    },

    /// A success whose body was not what the API documents.
    #[error("unexpected answer: {reason}")]
    Decode { reason: String },

    /// The session's MCP server answered a JSON-RPC error.
    #[error("MCP error {code}: {message}")]
    Mcp { code: i64, message: String },

    /// A tool reported that it failed (`isError`).
    #[error("tool {tool} failed: {message}")]
    Tool { tool: String, message: String },

    /// The session went to `failed` while it was waited for.
    #[error("session failed: {message}")]
    SessionFailed { message: String },
}

impl ComputerUseError {
    /// The HTTP status behind the error, when there is one.
    pub fn status(&self) -> Option<u16> {
        match self {
            Self::Unauthorized { .. } => Some(401),
            Self::PaymentRequired { .. } => Some(402),
            Self::Forbidden { .. } => Some(403),
            Self::NotFound { .. } => Some(404),
            Self::Conflict { .. } => Some(409),
            Self::InvalidPolicy { .. } => Some(422),
            Self::RateLimited { .. } => Some(429),
            Self::Api { status, .. } => Some(*status),
            _ => None,
        }
    }

    /// The API's machine-readable `code`, when it gave one (billing does).
    pub fn code(&self) -> Option<&str> {
        match self {
            Self::PaymentRequired { code, .. }
            | Self::Forbidden { code, .. }
            | Self::Conflict { code, .. }
            | Self::Api { code, .. } => code.as_deref(),
            _ => None,
        }
    }

    /// Where the account's owner puts a billing refusal right, when the API
    /// said.
    pub fn billing_url(&self) -> Option<&str> {
        match self {
            Self::PaymentRequired { billing_url, .. }
            | Self::Forbidden { billing_url, .. }
            | Self::Conflict { billing_url, .. } => billing_url.as_deref(),
            _ => None,
        }
    }

    pub(crate) fn configuration(reason: impl Into<String>) -> Self {
        Self::Configuration {
            reason: reason.into(),
        }
    }

    pub(crate) fn decode(reason: impl std::fmt::Display) -> Self {
        Self::Decode {
            reason: reason.to_string(),
        }
    }
}

/// A builder was asked to build without a required field.
#[derive(Debug, Clone, PartialEq, Eq, Error, uniffi::Error)]
pub enum BuildError {
    #[error("{record_type} is missing required field {field}")]
    MissingRequiredField { record_type: String, field: String },
}

impl BuildError {
    /// What the generated builders call.
    pub fn missing(record_type: &str, field: &str) -> Self {
        Self::MissingRequiredField {
            record_type: record_type.into(),
            field: field.into(),
        }
    }
}

impl From<BuildError> for ComputerUseError {
    fn from(error: BuildError) -> Self {
        Self::configuration(error.to_string())
    }
}

/// At most [`MAX_ERROR_BODY_BYTES`] of `body`, cut on a character.
pub(crate) fn bounded(body: &str) -> String {
    if body.len() <= MAX_ERROR_BODY_BYTES {
        return body.trim().to_owned();
    }
    let mut end = MAX_ERROR_BODY_BYTES;
    while !body.is_char_boundary(end) {
        end -= 1;
    }
    body[..end].trim().to_owned()
}
