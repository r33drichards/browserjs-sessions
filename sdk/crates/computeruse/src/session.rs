use crate::client::policy_body;
use crate::mcp::Mcp;
use crate::transport::{Call, Transport};
use crate::types::{
    Management, Policy, PolicyInput, RunJsRequest, RunJsResult, SessionInfo, SessionState,
    ToolInfo, ToolResult,
};
use crate::ComputerUseError;
use reqwest::Method;
use serde_json::{json, Value};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// How often a wait asks for the session's state.
const POLL_INTERVAL: Duration = Duration::from_secs(1);
/// A sleep answers when the snapshot is taken; the service allows that two
/// minutes.
const SLEEP_TIMEOUT: Duration = Duration::from_secs(180);
/// How long `wait_until` waits when it is given no limit.
const DEFAULT_WAIT: Duration = Duration::from_secs(300);

/// A handle to one session: its lifecycle, its policy and its MCP endpoint.
///
/// Made by [`Client::create_session`](crate::Client::create_session),
/// [`Client::get_session`](crate::Client::get_session) or
/// [`Client::session`](crate::Client::session).
#[derive(Debug, uniffi::Object)]
pub struct Session {
    transport: Arc<Transport>,
    id: String,
    /// What the API last said about the session.
    last: Mutex<Option<SessionInfo>>,
    mcp: Mcp,
}

impl Session {
    pub(crate) fn attach(
        transport: Arc<Transport>,
        id: String,
        info: Option<SessionInfo>,
    ) -> Arc<Self> {
        Arc::new(Self {
            mcp: Mcp::new(transport.clone(), &id),
            transport,
            id,
            last: Mutex::new(info),
        })
    }

    fn path(&self) -> String {
        format!("/v1/sessions/{}", self.id)
    }

    fn remember(&self, info: &SessionInfo) {
        *self.last.lock().unwrap_or_else(|p| p.into_inner()) = Some(info.clone());
    }

    async fn patch(&self, body: Value, operation: &str) -> Result<SessionInfo, ComputerUseError> {
        let info: SessionInfo = self
            .transport
            .send(Call::new(Method::PATCH, self.path(), operation).json(&body))
            .await?
            .json()?;
        self.remember(&info);
        Ok(info)
    }

    /// Stop or resume: `PATCH /v1/sessions/{id}` with `{"action": ...}`.
    async fn action(&self, action: &str) -> Result<SessionInfo, ComputerUseError> {
        self.patch(json!({"action": action}), action).await
    }
}

#[uniffi::export(async_runtime = "tokio")]
impl Session {
    /// The session's id.
    pub fn id(&self) -> String {
        self.id.clone()
    }

    /// The session's MCP endpoint on the API host, which takes a bearer
    /// token: `https://api.<domain>/<id>/mcp`.
    pub fn mcp_url(&self) -> String {
        format!("{}/{}/mcp", self.transport.base_url(), self.id)
    }

    /// What the API last said about the session, without asking again.
    /// `None` for a handle made by `Client::session` that has made no call
    /// yet.
    pub fn last_info(&self) -> Option<SessionInfo> {
        self.last.lock().unwrap_or_else(|p| p.into_inner()).clone()
    }

    /// Reads the session. Scope `sessions:read`.
    pub async fn refresh(&self) -> Result<SessionInfo, ComputerUseError> {
        let info: SessionInfo = self
            .transport
            .send(Call::new(Method::GET, self.path(), "get session"))
            .await?
            .json()?;
        self.remember(&info);
        Ok(info)
    }

    /// Renames the session: 1 to 63 characters. Scope `sessions:write`.
    pub async fn rename(&self, name: String) -> Result<SessionInfo, ComputerUseError> {
        self.patch(json!({"name": name}), "rename session").await
    }

    /// Changes the session's size (a name from
    /// [`Client::sizes`](crate::Client::sizes)). Scope `sessions:write`.
    ///
    /// A session that is asleep or stopped changes at once. One that is
    /// awake keeps running at its size, shows the new one as
    /// `pending_size`, and changes at its next start; asking for the size
    /// it runs at withdraws that. Either way **the next start is fresh**:
    /// the snapshot is dropped, so open windows and running programs are
    /// lost. The disk is kept. `400` for a size the deployment does not
    /// have.
    pub async fn resize(&self, size: String) -> Result<SessionInfo, ComputerUseError> {
        self.patch(json!({"size": size}), "resize session").await
    }

    /// Stops the session: the desktop goes, the disk stays, no snapshot is
    /// taken, and an MCP call does not wake it. Scope `sessions:write`.
    pub async fn stop(&self) -> Result<SessionInfo, ComputerUseError> {
        self.action("stop").await
    }

    /// Starts a stopped session from its disk, or wakes a sleeping one:
    /// the `resume` action, which [`Session::wake`] is a route for. Scope
    /// `sessions:write`. `402` where billing refuses it.
    pub async fn resume(&self) -> Result<SessionInfo, ComputerUseError> {
        self.action("resume").await
    }

    /// Puts a running session to sleep now instead of after its idle time:
    /// a snapshot of the running desktop is kept, compute is released, and
    /// the next MCP call (or [`Session::wake`]) brings it back as it was.
    /// Scope `sessions:write`.
    ///
    /// The answer comes when the snapshot is taken, which can take a
    /// minute or two; the call allows three minutes whatever the client's
    /// timeout. `state_saved` says whether there is a snapshot: without
    /// one the session still sleeps, and wakes from its disk. A session
    /// that is asleep already is left as it is. `409`
    /// ([`ComputerUseError::Conflict`]) for one that is starting, stopping,
    /// stopped or failed: only a running desktop has state to save.
    pub async fn sleep(&self) -> Result<SessionInfo, ComputerUseError> {
        let info: SessionInfo = self
            .transport
            .send(
                Call::new(Method::POST, format!("{}/sleep", self.path()), "sleep")
                    .min_timeout(SLEEP_TIMEOUT),
            )
            .await?
            .json()?;
        self.remember(&info);
        Ok(info)
    }

    /// Starts a session that is asleep or stopped, without making an MCP
    /// call: from its snapshot if it has one, from its disk otherwise. The
    /// answer does not wait for the desktop; see
    /// [`Session::wait_until_running`]. Waking one that is awake does
    /// nothing. Scope `sessions:write`. `402` where billing refuses it.
    pub async fn wake(&self) -> Result<SessionInfo, ComputerUseError> {
        let info: SessionInfo = self
            .transport
            .send(Call::new(
                Method::POST,
                format!("{}/wake", self.path()),
                "wake",
            ))
            .await?
            .json()?;
        self.remember(&info);
        Ok(info)
    }

    /// Deletes the session, its disk and its snapshot. This cannot be
    /// undone. Scope `sessions:write`. Deleting one that is already gone
    /// succeeds.
    pub async fn delete(&self) -> Result<(), ComputerUseError> {
        self.transport
            .send(Call::new(Method::DELETE, self.path(), "delete session"))
            .await?;
        Ok(())
    }

    /// Polls until the session is in `state`. `timeout_ms` defaults to five
    /// minutes. Fails with [`ComputerUseError::SessionFailed`] if the
    /// session goes to `failed` instead, and with
    /// [`ComputerUseError::Timeout`] when the time runs out.
    pub async fn wait_until(
        &self,
        state: SessionState,
        timeout_ms: Option<u64>,
    ) -> Result<SessionInfo, ComputerUseError> {
        let limit = timeout_ms
            .map(Duration::from_millis)
            .unwrap_or(DEFAULT_WAIT);
        let started = Instant::now();
        loop {
            let info = self.refresh().await?;
            if info.state == state {
                return Ok(info);
            }
            if info.state == SessionState::Failed {
                return Err(ComputerUseError::SessionFailed {
                    message: info.message.unwrap_or_else(|| "no reason given".into()),
                });
            }
            if started.elapsed() + POLL_INTERVAL > limit {
                return Err(ComputerUseError::Timeout {
                    operation: format!(
                        "session {} is {:?}, not {:?}, after {}s",
                        self.id,
                        info.state,
                        state,
                        limit.as_secs()
                    ),
                });
            }
            tokio::time::sleep(POLL_INTERVAL).await;
        }
    }

    /// [`Session::wait_until`] for `running`.
    pub async fn wait_until_running(
        &self,
        timeout_ms: Option<u64>,
    ) -> Result<SessionInfo, ComputerUseError> {
        self.wait_until(SessionState::Running, timeout_ms).await
    }

    /// The session's policy, with its source. Scope `policies:read`. `409`
    /// for a session that predates policies.
    pub async fn policy(&self) -> Result<Policy, ComputerUseError> {
        self.transport
            .send(Call::new(
                Method::GET,
                format!("{}/policy", self.path()),
                "get policy",
            ))
            .await?
            .json()
    }

    /// Replaces the policy. Scope `policies:write`.
    ///
    /// The answer's `state` is `ready` when the policy is in force
    /// everywhere and `loading` when it is saved and still being loaded.
    /// `422` ([`ComputerUseError::InvalidPolicy`]) saves nothing. `409`
    /// when the policy is managed in the app's editor and `management`
    /// does not move it to `iac`. `412` when `if_match_version` is not the
    /// current version.
    pub async fn put_policy(&self, policy: PolicyInput) -> Result<Policy, ComputerUseError> {
        let mut call = Call::new(Method::PUT, format!("{}/policy", self.path()), "put policy")
            .json(&policy_body(&policy));
        if let Some(version) = policy.if_match_version {
            call = call.header("If-Match", format!("\"{version}\""));
        }
        self.transport.send(call).await?.json()
    }

    /// Returns the policy to unrestricted, in `editor` mode. Scope
    /// `policies:write`.
    pub async fn reset_policy(&self) -> Result<Policy, ComputerUseError> {
        self.transport
            .send(Call::new(
                Method::DELETE,
                format!("{}/policy", self.path()),
                "reset policy",
            ))
            .await?
            .json()
    }

    /// Changes who manages the policy. Scope `policies:write`.
    pub async fn set_policy_management(
        &self,
        management: Management,
    ) -> Result<Policy, ComputerUseError> {
        let body = serde_json::to_value(&management).map_err(ComputerUseError::decode)?;
        self.transport
            .send(
                Call::new(
                    Method::PUT,
                    format!("{}/policy/management", self.path()),
                    "set policy management",
                )
                .json(&body),
            )
            .await?
            .json()
    }

    /// Runs JavaScript or TypeScript in the session with the `run_js` tool
    /// and returns what it printed. Scope `sessions:connect`.
    ///
    /// The call wakes a sleeping session and waits for it. A stopped one
    /// answers `409`. A program that throws is not an error of the call:
    /// see [`RunJsResult::error`].
    pub async fn run_js(&self, code: String) -> Result<RunJsResult, ComputerUseError> {
        self.run_js_with(RunJsRequest {
            code,
            heap_memory_max_mb: None,
            execution_timeout_secs: None,
        })
        .await
    }

    /// [`Session::run_js`] with a heap limit and a time limit.
    pub async fn run_js_with(
        &self,
        request: RunJsRequest,
    ) -> Result<RunJsResult, ComputerUseError> {
        let mut arguments = json!({"code": request.code});
        if let Some(heap) = request.heap_memory_max_mb {
            arguments["heap_memory_max_mb"] = json!(heap);
        }
        if let Some(seconds) = request.execution_timeout_secs {
            arguments["execution_timeout_secs"] = json!(seconds);
        }
        let result = self.mcp.call_tool("run_js", arguments).await?;
        Ok(run_js_result(result))
    }

    /// Calls any tool of the session's MCP server. `arguments_json` is the
    /// tool's arguments as a JSON object; `None` for none. Scope
    /// `sessions:connect`. A tool that reports failure is
    /// [`ComputerUseError::Tool`].
    pub async fn call_tool(
        &self,
        name: String,
        arguments_json: Option<String>,
    ) -> Result<ToolResult, ComputerUseError> {
        let arguments = match arguments_json {
            Some(text) => {
                let value: Value = serde_json::from_str(&text).map_err(|error| {
                    ComputerUseError::configuration(format!("arguments_json is not JSON: {error}"))
                })?;
                if !value.is_object() {
                    return Err(ComputerUseError::configuration(
                        "arguments_json must be a JSON object",
                    ));
                }
                value
            }
            None => json!({}),
        };
        self.mcp.call_tool(&name, arguments).await
    }

    /// The tools of the session's MCP server. Scope `sessions:connect`.
    pub async fn list_tools(&self) -> Result<Vec<ToolInfo>, ComputerUseError> {
        self.mcp.list_tools().await
    }
}

/// `run_js` answers a JSON text block, `{"output", "error"?, "artifacts"?}`,
/// and then one block per artifact.
fn run_js_result(result: ToolResult) -> RunJsResult {
    let mut blocks = result.content.into_iter();
    let first = blocks.next();
    let raw = first
        .as_ref()
        .and_then(|block| block.text.clone())
        .unwrap_or_default();
    let parsed: Option<Value> = serde_json::from_str(&raw).ok().filter(Value::is_object);
    let field = |name: &str| {
        parsed
            .as_ref()
            .and_then(|json| json.get(name))
            .and_then(Value::as_str)
            .map(str::to_owned)
    };
    let mut artifacts: Vec<_> = blocks.collect();
    let output = match &parsed {
        Some(_) => field("output").unwrap_or_default(),
        // Not the JSON expected: show what came rather than nothing.
        None => {
            if let Some(block) = first.filter(|block| block.text.is_none()) {
                artifacts.insert(0, block);
            }
            raw.clone()
        }
    };
    RunJsResult {
        output,
        error: field("error"),
        artifacts,
        raw_json: raw,
    }
}
