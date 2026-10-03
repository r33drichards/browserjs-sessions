//! The Rust-only surface: builders that borrow and take `impl Into`, where
//! the exported one takes owned records.

use crate::types::{ClientOptions, CreateSessionRequest, PolicyInput, SessionInfo};
use crate::{Client, ComputerUseError, Session};
use std::sync::Arc;
use std::time::Duration;

/// Builds a [`Client`]. From [`Client::builder`].
///
/// Its `Debug` does not print the token.
#[derive(Debug, Clone, Default)]
#[must_use = "a builder does nothing until build() is called"]
pub struct ClientBuilder {
    options: Option<ClientOptions>,
}

impl ClientBuilder {
    fn options(&mut self) -> &mut ClientOptions {
        self.options.get_or_insert_with(|| ClientOptions {
            api_token: String::new(),
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

    /// The API token (`bjs_<id>_<secret>`), or an access token made from
    /// one. Required.
    pub fn api_token(mut self, token: impl Into<String>) -> Self {
        self.options().api_token = token.into();
        self
    }

    /// The API host. Default `https://api.computeruse.site`.
    pub fn base_url(mut self, url: impl Into<String>) -> Self {
        self.options().base_url = Some(url.into());
        self
    }

    /// Narrows the access token to these scopes.
    pub fn scopes<I, S>(mut self, scopes: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.options().scopes = Some(scopes.into_iter().map(Into::into).collect());
        self
    }

    /// Whether the API token is exchanged for a short-lived access token
    /// (the default) or sent on every request.
    pub fn exchange_token(mut self, exchange: bool) -> Self {
        self.options().exchange_token = Some(exchange);
        self
    }

    /// The time limit of one API request. Default 60 seconds.
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.options().timeout_ms = Some(as_millis(timeout));
        self
    }

    /// The time limit of one MCP request. Default 630 seconds.
    pub fn mcp_timeout(mut self, timeout: Duration) -> Self {
        self.options().mcp_timeout_ms = Some(as_millis(timeout));
        self
    }

    /// How many times a request is tried again. Default 3; 0 for never.
    pub fn max_retries(mut self, retries: u32) -> Self {
        self.options().max_retries = Some(retries);
        self
    }

    /// The first pause between tries. Default 500 milliseconds.
    pub fn retry_base_delay(mut self, delay: Duration) -> Self {
        self.options().retry_base_delay_ms = Some(as_millis(delay));
        self
    }

    /// How long an MCP call keeps trying while the session wakes. Default
    /// 600 seconds.
    pub fn wake_timeout(mut self, timeout: Duration) -> Self {
        self.options().wake_timeout_ms = Some(as_millis(timeout));
        self
    }

    /// Added in front of the SDK's own `User-Agent`.
    pub fn user_agent(mut self, user_agent: impl Into<String>) -> Self {
        self.options().user_agent = Some(user_agent.into());
        self
    }

    /// Accept an `http` base URL that is not a loopback address. The token
    /// then crosses the network in the clear.
    pub fn allow_insecure_http(mut self, allow: bool) -> Self {
        self.options().allow_insecure_http = Some(allow);
        self
    }

    /// The client. Fails with [`ComputerUseError::Configuration`] for a
    /// missing token or a base URL that is not an https host.
    pub fn build(mut self) -> Result<Arc<Client>, ComputerUseError> {
        let options = self.options().clone();
        Client::new(options)
    }
}

fn as_millis(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

impl Client {
    /// Starts a client: `Client::builder().api_token(token).build()?`.
    pub fn builder() -> ClientBuilder {
        ClientBuilder::default()
    }

    /// The session calls, grouped: `client.sessions().create().send()`.
    pub fn sessions(&self) -> Sessions<'_> {
        Sessions { client: self }
    }
}

/// The session calls of a [`Client`]. From [`Client::sessions`].
#[derive(Debug, Clone, Copy)]
pub struct Sessions<'a> {
    client: &'a Client,
}

impl<'a> Sessions<'a> {
    /// The caller's sessions.
    pub async fn list(&self) -> Result<Vec<SessionInfo>, ComputerUseError> {
        self.client.list_sessions().await
    }

    /// Reads a session and returns a handle to it.
    pub async fn get(&self, id: impl Into<String>) -> Result<Arc<Session>, ComputerUseError> {
        self.client.get_session(id.into()).await
    }

    /// A handle to a session, without asking whether it exists.
    pub fn handle(&self, id: impl Into<String>) -> Result<Arc<Session>, ComputerUseError> {
        self.client.session(id.into())
    }

    /// Starts a create: `.name("x").policy_preset("unrestricted").send()`.
    pub fn create(&self) -> CreateSession<'a> {
        CreateSession {
            client: self.client,
            request: CreateSessionRequest::default(),
        }
    }
}

/// A session being created. From [`Sessions::create`]. Nothing is sent
/// until [`CreateSession::send`].
#[derive(Debug, Clone)]
#[must_use = "nothing is created until send() is awaited"]
pub struct CreateSession<'a> {
    client: &'a Client,
    request: CreateSessionRequest,
}

impl CreateSession<'_> {
    /// The session's name: 1 to 63 characters. Left out, the service
    /// picks one.
    pub fn name(mut self, name: impl Into<String>) -> Self {
        self.request.name = Some(name.into());
        self
    }

    /// The session's policy.
    pub fn policy(mut self, policy: PolicyInput) -> Self {
        self.request.policy = Some(policy);
        self
    }

    /// The session's policy, as Rego source.
    pub fn policy_rego(self, source: impl Into<String>) -> Self {
        self.policy(PolicyInput {
            source: source.into(),
            management: None,
            if_match_version: None,
        })
    }

    /// The session's policy, as the id of a preset such as `unrestricted`.
    pub fn policy_preset(mut self, id: impl Into<String>) -> Self {
        self.request.policy_preset = Some(id.into());
        self
    }

    /// The session's size, such as `medium`. Left out: the default.
    pub fn size(mut self, size: impl Into<String>) -> Self {
        self.request.size = Some(size.into());
        self
    }

    /// Creates the session.
    pub async fn send(self) -> Result<Arc<Session>, ComputerUseError> {
        self.client.create_session(self.request).await
    }
}
