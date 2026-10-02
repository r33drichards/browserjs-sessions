//! A small MCP client for one session's endpoint, over Streamable HTTP:
//! `initialize`, `tools/list` and `tools/call`, each a `POST` answered with
//! JSON or with an event stream that carries the answer.

use crate::error::bounded;
use crate::transport::{Call, Retry, Transport};
use crate::types::{ContentBlock, ToolInfo, ToolResult};
use crate::ComputerUseError;
use base64::Engine;
use reqwest::header::CONTENT_TYPE;
use reqwest::Method;
use serde_json::{json, Value};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use tokio::sync::Mutex;

const PROTOCOL_VERSION: &str = "2025-06-18";
const ACCEPT: &str = "application/json, text/event-stream";

/// What `initialize` settled.
#[derive(Debug, Clone)]
struct Handshake {
    /// `Mcp-Session-Id`, if the server gave one.
    session_id: Option<String>,
    protocol_version: String,
}

#[derive(Debug)]
pub(crate) struct Mcp {
    transport: Arc<Transport>,
    path: String,
    next_id: AtomicU64,
    /// Held only while the handshake is made, never across a tool call.
    handshake: Mutex<Option<Handshake>>,
}

impl Mcp {
    pub(crate) fn new(transport: Arc<Transport>, session_id: &str) -> Self {
        Self {
            transport,
            path: format!("/{session_id}/mcp"),
            next_id: AtomicU64::new(1),
            handshake: Mutex::new(None),
        }
    }

    pub(crate) async fn call_tool(
        &self,
        name: &str,
        arguments: Value,
    ) -> Result<ToolResult, ComputerUseError> {
        let result = self
            .request("tools/call", json!({"name": name, "arguments": arguments}))
            .await?;
        let content: Vec<ContentBlock> = result
            .get("content")
            .and_then(Value::as_array)
            .map(|blocks| blocks.iter().map(content_block).collect())
            .unwrap_or_default();
        // A tool that reports failure (`isError`) is an error already: post.
        Ok(ToolResult {
            content,
            structured_json: result.get("structuredContent").map(Value::to_string),
        })
    }

    pub(crate) async fn list_tools(&self) -> Result<Vec<ToolInfo>, ComputerUseError> {
        let mut tools = Vec::new();
        let mut cursor: Option<String> = None;
        loop {
            let params = match &cursor {
                Some(cursor) => json!({"cursor": cursor}),
                None => json!({}),
            };
            let result = self.request("tools/list", params).await?;
            for tool in result
                .get("tools")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                tools.push(ToolInfo {
                    name: tool
                        .get("name")
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                    description: tool
                        .get("description")
                        .and_then(Value::as_str)
                        .map(str::to_owned),
                    input_schema_json: tool
                        .get("inputSchema")
                        .map(Value::to_string)
                        .unwrap_or_else(|| "{}".into()),
                });
            }
            cursor = result
                .get("nextCursor")
                .and_then(Value::as_str)
                .filter(|c| !c.is_empty())
                .map(str::to_owned);
            if cursor.is_none() {
                return Ok(tools);
            }
        }
    }

    /// One request after the handshake. A server that has forgotten the
    /// MCP session (the desktop restarted: a stop and resume, or a wake
    /// from disk) answers 404 to its id; the handshake is then made again,
    /// once.
    async fn request(&self, method: &str, params: Value) -> Result<Value, ComputerUseError> {
        let handshake = self.handshake().await?;
        match self.rpc(method, params.clone(), Some(&handshake)).await {
            Err(ComputerUseError::NotFound { .. }) if handshake.session_id.is_some() => {
                self.forget(&handshake).await;
                let handshake = self.handshake().await?;
                self.rpc(method, params, Some(&handshake)).await
            }
            other => other,
        }
    }

    async fn forget(&self, stale: &Handshake) {
        let mut current = self.handshake.lock().await;
        if current.as_ref().map(|h| &h.session_id) == Some(&stale.session_id) {
            *current = None;
        }
    }

    async fn handshake(&self) -> Result<Handshake, ComputerUseError> {
        let mut current = self.handshake.lock().await;
        if let Some(handshake) = current.as_ref() {
            return Ok(handshake.clone());
        }
        let (result, session_id) = self
            .post(
                "initialize",
                Some(json!({
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {},
                    "clientInfo": {"name": "computeruse-sdk", "version": env!("CARGO_PKG_VERSION")},
                })),
                None,
                true,
            )
            .await?;
        let handshake = Handshake {
            session_id,
            protocol_version: result
                .get("protocolVersion")
                .and_then(Value::as_str)
                .unwrap_or(PROTOCOL_VERSION)
                .to_owned(),
        };
        self.post("notifications/initialized", None, Some(&handshake), false)
            .await?;
        *current = Some(handshake.clone());
        Ok(handshake)
    }

    async fn rpc(
        &self,
        method: &str,
        params: Value,
        handshake: Option<&Handshake>,
    ) -> Result<Value, ComputerUseError> {
        Ok(self.post(method, Some(params), handshake, true).await?.0)
    }

    /// Posts one JSON-RPC message. `expects_answer` is false for a
    /// notification. Returns the result and the `Mcp-Session-Id` header.
    async fn post(
        &self,
        method: &str,
        params: Option<Value>,
        handshake: Option<&Handshake>,
        expects_answer: bool,
    ) -> Result<(Value, Option<String>), ComputerUseError> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let mut message = json!({"jsonrpc": "2.0", "method": method});
        if expects_answer {
            message["id"] = json!(id);
        }
        if let Some(params) = params {
            message["params"] = params;
        }

        let operation = format!("mcp {method}");
        let mut call = Call::new(Method::POST, self.path.clone(), &operation)
            .json(&message)
            .header("Accept", ACCEPT)
            .retry(Retry::Mcp);
        if let Some(handshake) = handshake {
            call = call.header("MCP-Protocol-Version", handshake.protocol_version.clone());
            if let Some(session_id) = &handshake.session_id {
                call = call.header("Mcp-Session-Id", session_id.clone());
            }
        }
        let answer = self.transport.send(call).await?;
        let session_id = answer
            .headers
            .get("mcp-session-id")
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned);
        if !expects_answer {
            return Ok((Value::Null, session_id));
        }

        let event_stream = answer
            .headers
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .is_some_and(|value| value.starts_with("text/event-stream"));
        let text = String::from_utf8_lossy(&answer.body);
        let reply = if event_stream {
            sse_reply(&text, id)
        } else {
            serde_json::from_str::<Value>(&text).ok()
        }
        .ok_or_else(|| ComputerUseError::Decode {
            reason: format!(
                "{operation}: no JSON-RPC answer in the response: {}",
                bounded(&text.chars().take(200).collect::<String>())
            ),
        })?;

        if let Some(error) = reply.get("error") {
            return Err(ComputerUseError::Mcp {
                code: error.get("code").and_then(Value::as_i64).unwrap_or(0),
                message: bounded(
                    error
                        .get("message")
                        .and_then(Value::as_str)
                        .unwrap_or("no message"),
                ),
            });
        }
        let result = reply.get("result").cloned().ok_or_else(|| {
            ComputerUseError::decode(format!("{operation}: the answer has no result"))
        })?;
        if method == "tools/call" && result.get("isError").and_then(Value::as_bool) == Some(true) {
            let name = message["params"]["name"].as_str().unwrap_or_default();
            let text = result
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|block| block.get("text").and_then(Value::as_str))
                .collect::<Vec<_>>()
                .join("\n");
            return Err(ComputerUseError::Tool {
                tool: name.to_owned(),
                message: bounded(&text),
            });
        }
        Ok((result, session_id))
    }
}

/// The JSON-RPC answer to request `id` in an event stream: the `data` of
/// the event that carries that id. Other events (notifications, requests
/// from the server) are passed over.
fn sse_reply(stream: &str, id: u64) -> Option<Value> {
    let mut data = String::new();
    let mut found = None;
    let mut flush = |data: &mut String| {
        if !data.is_empty() {
            if let Ok(value) = serde_json::from_str::<Value>(data) {
                let is_answer = value.get("result").is_some() || value.get("error").is_some();
                if is_answer && value.get("id").and_then(Value::as_u64) == Some(id) {
                    found = Some(value);
                }
            }
            data.clear();
        }
    };
    for line in stream.lines() {
        if line.is_empty() {
            flush(&mut data);
        } else if let Some(rest) = line.strip_prefix("data:") {
            if !data.is_empty() {
                data.push('\n');
            }
            data.push_str(rest.strip_prefix(' ').unwrap_or(rest));
        }
    }
    flush(&mut data);
    found
}

fn content_block(block: &Value) -> ContentBlock {
    let text = |name: &str| block.get(name).and_then(Value::as_str).map(str::to_owned);
    ContentBlock {
        kind: text("type").unwrap_or_default(),
        text: text("text"),
        data: text("data").and_then(|encoded| {
            base64::engine::general_purpose::STANDARD
                .decode(encoded.as_bytes())
                .ok()
        }),
        mime_type: text("mimeType"),
        raw_json: block.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn answer_in_an_event_stream() {
        let stream =
            "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n\
                      id: 7\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\n\
                      data: \"result\":{\"ok\":true}}\n\n";
        assert_eq!(sse_reply(stream, 3).unwrap()["result"]["ok"], true);
        assert!(sse_reply(stream, 4).is_none());
        // No trailing blank line, and CRLF.
        let stream = "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\r\n";
        assert!(sse_reply(stream, 1).is_some());
    }

    #[test]
    fn blocks() {
        let image =
            content_block(&json!({"type": "image", "data": "aGk=", "mimeType": "image/png"}));
        assert_eq!(image.kind, "image");
        assert_eq!(image.data.as_deref(), Some(&b"hi"[..]));
        assert_eq!(image.mime_type.as_deref(), Some("image/png"));
        let text = content_block(&json!({"type": "text", "text": "hello"}));
        assert_eq!(text.text.as_deref(), Some("hello"));
        assert!(text.data.is_none());
    }
}
