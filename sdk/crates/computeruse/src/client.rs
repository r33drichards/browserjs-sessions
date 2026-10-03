use crate::session::Session;
use crate::transport::{Call, Retry, Transport};
use crate::types::{
    ClientOptions, CreateSessionRequest, Evaluation, Me, PolicyInput, PolicyPreset, SessionInfo,
    SessionSizes, Validation,
};
use crate::ComputerUseError;
use reqwest::Method;
use serde_json::{json, Value};
use std::sync::Arc;

/// A client for the Computer Use API with one token.
///
/// Cheap to share: clone the `Arc`. Its `Debug` does not print the token.
#[derive(Debug, uniffi::Object)]
pub struct Client {
    pub(crate) transport: Arc<Transport>,
}

#[uniffi::export(async_runtime = "tokio")]
impl Client {
    /// A client made from `options`. Nothing is sent until the first call.
    #[uniffi::constructor]
    pub fn new(options: ClientOptions) -> Result<Arc<Self>, ComputerUseError> {
        Ok(Arc::new(Self {
            transport: Arc::new(Transport::new(&options)?),
        }))
    }

    /// A client for the hosted service with every default.
    #[uniffi::constructor]
    pub fn with_token(api_token: String) -> Result<Arc<Self>, ComputerUseError> {
        Self::new(ClientOptions {
            api_token,
            base_url: None,
            scopes: None,
            exchange_token: None,
            timeout_ms: None,
            mcp_timeout_ms: None,
            max_retries: None,
            retry_base_delay_ms: None,
            wake_timeout_ms: None,
            user_agent: None,
            allow_insecure_http: None,
        })
    }

    /// The API host this client talks to, without a trailing slash.
    pub fn base_url(&self) -> String {
        self.transport.base_url().to_owned()
    }

    /// The bearer token the next request would carry, for a caller that
    /// opens its own connection (an MCP client of its own, say). It is the
    /// short-lived access token unless `exchange_token` is off.
    /// `force_refresh` asks for a new one. Treat the value as a secret.
    pub async fn access_token(&self, force_refresh: bool) -> Result<String, ComputerUseError> {
        self.transport.bearer(force_refresh).await
    }

    /// Who the token acts as. Needs no scope.
    pub async fn me(&self) -> Result<Me, ComputerUseError> {
        self.transport
            .send(Call::new(Method::GET, "/v1/me", "me"))
            .await?
            .json()
    }

    /// The caller's sessions. Scope `sessions:read`; refused (403) for a
    /// token bound to one session.
    pub async fn list_sessions(&self) -> Result<Vec<SessionInfo>, ComputerUseError> {
        self.transport
            .send(Call::new(Method::GET, "/v1/sessions", "list sessions"))
            .await?
            .json_list()
    }

    /// Creates a session. Scope `sessions:write`.
    ///
    /// The answer does not wait for the desktop: the session is `starting`.
    /// An MCP call waits for it; so does
    /// [`Session::wait_until_running`].
    ///
    /// `409` ([`ComputerUseError::Conflict`]) at the session limit, and with
    /// the code `no_capacity` when there is no room for the size asked for
    /// (nothing is created; try later or smaller). `400` for a size the
    /// deployment does not have. `422`
    /// ([`ComputerUseError::InvalidPolicy`]) for a policy that does not
    /// validate, in which case nothing was created.
    pub async fn create_session(
        &self,
        request: CreateSessionRequest,
    ) -> Result<Arc<Session>, ComputerUseError> {
        let policy = match (request.policy, request.policy_preset) {
            (Some(_), Some(_)) => {
                return Err(ComputerUseError::configuration(
                    "give a session a policy or a policy_preset, not both",
                ));
            }
            (Some(policy), None) => Some(policy_body(&policy)),
            (None, Some(preset)) => {
                let presets = self.policy_presets().await?;
                let found = presets
                    .into_iter()
                    .find(|p| p.id == preset)
                    .ok_or_else(|| {
                        ComputerUseError::configuration(format!(
                            "there is no policy preset {preset:?}"
                        ))
                    })?;
                Some(json!({"kind": found.kind, "source": found.source}))
            }
            (None, None) => None,
        };

        let mut body = serde_json::Map::new();
        if let Some(name) = request.name {
            body.insert("name".into(), Value::String(name));
        }
        if let Some(policy) = policy {
            body.insert("policy".into(), policy);
        }
        if let Some(size) = request.size {
            body.insert("size".into(), Value::String(size));
        }
        let info: SessionInfo = self
            .transport
            .send(
                Call::new(Method::POST, "/v1/sessions", "create session")
                    .json(&Value::Object(body))
                    .retry(Retry::Create),
            )
            .await?
            .json()?;
        Ok(Session::attach(
            self.transport.clone(),
            info.id.clone(),
            Some(info),
        ))
    }

    /// Reads a session and returns a handle to it. Scope `sessions:read`.
    /// `404` for one that does not exist or is not the caller's.
    pub async fn get_session(&self, id: String) -> Result<Arc<Session>, ComputerUseError> {
        let session = self.session(id)?;
        session.refresh().await?;
        Ok(session)
    }

    /// A handle to the session `id`, without asking the API whether it
    /// exists. Enough for a token with `sessions:connect` alone.
    pub fn session(&self, id: String) -> Result<Arc<Session>, ComputerUseError> {
        if !valid_session_id(&id) {
            return Err(ComputerUseError::configuration(format!(
                "{id:?} is not a session id (s- and five or ten characters)"
            )));
        }
        Ok(Session::attach(self.transport.clone(), id, None))
    }

    /// The sizes a session can have here, and the default. Needs no scope.
    pub async fn sizes(&self) -> Result<SessionSizes, ComputerUseError> {
        self.transport
            .send(Call::new(Method::GET, "/v1/sizes", "list sizes"))
            .await?
            .json()
    }

    /// The ready-made policies, `unrestricted` first. Needs no scope.
    pub async fn policy_presets(&self) -> Result<Vec<PolicyPreset>, ComputerUseError> {
        self.transport
            .send(Call::new(
                Method::GET,
                "/v1/policy-presets",
                "list policy presets",
            ))
            .await?
            .json_list()
    }

    /// Checks a Rego policy without saving it. An invalid policy is not an
    /// error here: it is a [`Validation`] whose `ok` is false. Needs no
    /// scope.
    pub async fn validate_policy(&self, source: String) -> Result<Validation, ComputerUseError> {
        self.transport
            .send(
                Call::new(Method::POST, "/v1/policies/validate", "validate policy")
                    .json(&json!({"kind": "rego", "source": source})),
            )
            .await?
            .json()
    }

    /// Asks a Rego policy about one sample call. `input_json` is the input
    /// document, as JSON. Needs no scope.
    pub async fn evaluate_policy(
        &self,
        source: String,
        input_json: String,
    ) -> Result<Evaluation, ComputerUseError> {
        let input: Value = serde_json::from_str(&input_json).map_err(|error| {
            ComputerUseError::configuration(format!("input_json is not JSON: {error}"))
        })?;
        self.transport
            .send(
                Call::new(Method::POST, "/v1/policies/evaluate", "evaluate policy")
                    .json(&json!({"kind": "rego", "source": source, "input": input})),
            )
            .await?
            .json()
    }
}

/// The body of a policy write. Rego is the only kind; it is named for
/// backends that still ask.
pub(crate) fn policy_body(policy: &PolicyInput) -> Value {
    let mut body = json!({"kind": "rego", "source": policy.source});
    if let Some(management) = &policy.management {
        body["management"] = serde_json::to_value(management).unwrap_or(Value::Null);
    }
    body
}

/// `s-` and ten characters of base32, or five of lower-case letters and
/// digits. Checked here because the id becomes part of a URL's path.
pub(crate) fn valid_session_id(id: &str) -> bool {
    let Some(rest) = id.strip_prefix("s-") else {
        return false;
    };
    match rest.len() {
        10 => rest
            .bytes()
            .all(|b| b.is_ascii_lowercase() || (b'2'..=b'7').contains(&b)),
        5 => rest
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit()),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_ids() {
        assert!(valid_session_id("s-abcde"));
        assert!(valid_session_id("s-ab1de"));
        assert!(valid_session_id("s-abcdefghij"));
        assert!(valid_session_id("s-abcdef2345"));
        for bad in [
            "",
            "s-",
            "abcde",
            "s-ABCDE",
            "s-abc/e",
            "s-abcdefgh1j",
            "s-../v1",
        ] {
            assert!(!valid_session_id(bad), "{bad}");
        }
    }
}
