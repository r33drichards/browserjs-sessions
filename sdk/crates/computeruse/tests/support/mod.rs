//! An in-process fake of the API host: a tiny HTTP/1.1 server that records
//! what it was sent and answers with what the test's handler returns.
#![allow(dead_code)]

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// The API token the tests use. It has the shape of a real one and is not
/// one: the id and the secret are made up.
pub const TOKEN: &str = "bjs_aaaaaaaaaaaa_c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0";
pub const TOKEN_ID: &str = "aaaaaaaaaaaa";

#[derive(Debug, Clone)]
pub struct Request {
    pub method: String,
    pub path: String,
    /// Names in lower case.
    pub headers: HashMap<String, String>,
    pub body: String,
}

impl Request {
    pub fn header(&self, name: &str) -> &str {
        self.headers.get(name).map(String::as_str).unwrap_or("")
    }

    pub fn is(&self, method: &str, path: &str) -> bool {
        self.method == method && self.path == path
    }

    pub fn json(&self) -> serde_json::Value {
        serde_json::from_str(&self.body).unwrap_or(serde_json::Value::Null)
    }
}

#[derive(Debug, Clone)]
pub struct Reply {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: String,
}

impl Reply {
    pub fn json(status: u16, body: serde_json::Value) -> Self {
        Self {
            status,
            headers: vec![("content-type".into(), "application/json".into())],
            body: body.to_string(),
        }
    }

    pub fn text(status: u16, body: &str) -> Self {
        Self {
            status,
            headers: vec![("content-type".into(), "text/plain; charset=utf-8".into())],
            body: body.into(),
        }
    }

    pub fn empty(status: u16) -> Self {
        Self {
            status,
            headers: Vec::new(),
            body: String::new(),
        }
    }

    /// A JSON-RPC answer in an event stream, as mcp-js sends it.
    pub fn sse(message: serde_json::Value) -> Self {
        Self {
            status: 200,
            headers: vec![("content-type".into(), "text/event-stream".into())],
            body: format!("event: message\ndata: {message}\n\n"),
        }
    }

    pub fn header(mut self, name: &str, value: &str) -> Self {
        self.headers.push((name.into(), value.into()));
        self
    }
}

pub struct Fake {
    pub url: String,
    requests: Arc<Mutex<Vec<Request>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Fake {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Fake {
    /// Starts a server on a free loopback port. `handler` sees each
    /// request and how many came before it.
    pub async fn start<H>(handler: H) -> Self
    where
        H: Fn(&Request, usize) -> Reply + Send + Sync + 'static,
    {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let requests = Arc::new(Mutex::new(Vec::new()));
        let recorded = requests.clone();
        let handler = Arc::new(handler);
        let task = tokio::spawn(async move {
            loop {
                let Ok((mut socket, _)) = listener.accept().await else {
                    return;
                };
                let recorded = recorded.clone();
                let handler = handler.clone();
                tokio::spawn(async move {
                    let Some(request) = read_request(&mut socket).await else {
                        return;
                    };
                    let reply = {
                        let mut all = recorded.lock().unwrap();
                        let reply = handler(&request, all.len());
                        all.push(request);
                        reply
                    };
                    let mut head = format!(
                        "HTTP/1.1 {} Fake\r\ncontent-length: {}\r\nconnection: close\r\n",
                        reply.status,
                        reply.body.len()
                    );
                    for (name, value) in &reply.headers {
                        head.push_str(&format!("{name}: {value}\r\n"));
                    }
                    head.push_str("\r\n");
                    let _ = socket.write_all(head.as_bytes()).await;
                    let _ = socket.write_all(reply.body.as_bytes()).await;
                    let _ = socket.shutdown().await;
                });
            }
        });
        Self {
            url,
            requests,
            task,
        }
    }

    /// A fake that also plays the token endpoint: every exchange is given
    /// the access token `access-<n>`, n counting from 1. `handler` sees
    /// everything else.
    pub async fn api<H>(handler: H) -> Self
    where
        H: Fn(&Request) -> Reply + Send + Sync + 'static,
    {
        let exchanges = Mutex::new(0);
        Self::start(move |request, _| {
            if request.is("POST", "/oauth/token") {
                let mut n = exchanges.lock().unwrap();
                *n += 1;
                return Reply::json(
                    200,
                    serde_json::json!({
                        "access_token": format!("access-{n}"),
                        "token_type": "Bearer",
                        "expires_in": 3600,
                        "scope": "sessions:read sessions:write sessions:connect",
                    }),
                );
            }
            handler(request)
        })
        .await
    }

    pub fn requests(&self) -> Vec<Request> {
        self.requests.lock().unwrap().clone()
    }

    /// The requests that are not token exchanges, as `METHOD path`.
    pub fn calls(&self) -> Vec<String> {
        self.requests()
            .iter()
            .filter(|r| r.path != "/oauth/token")
            .map(|r| format!("{} {}", r.method, r.path))
            .collect()
    }

    pub fn count(&self, method: &str, path: &str) -> usize {
        self.requests()
            .iter()
            .filter(|r| r.is(method, path))
            .count()
    }
}

async fn read_request(socket: &mut tokio::net::TcpStream) -> Option<Request> {
    let mut buffer = Vec::new();
    let mut chunk = [0u8; 4096];
    let head_end = loop {
        if let Some(position) = buffer.windows(4).position(|w| w == b"\r\n\r\n") {
            break position;
        }
        let read = socket.read(&mut chunk).await.ok()?;
        if read == 0 {
            return None;
        }
        buffer.extend_from_slice(&chunk[..read]);
    };
    let head = String::from_utf8_lossy(&buffer[..head_end]).into_owned();
    let mut lines = head.split("\r\n");
    let mut first = lines.next()?.split(' ');
    let method = first.next()?.to_owned();
    let path = first.next()?.to_owned();
    let headers: HashMap<String, String> = lines
        .filter_map(|line| line.split_once(':'))
        .map(|(name, value)| (name.trim().to_ascii_lowercase(), value.trim().to_owned()))
        .collect();
    let length: usize = headers
        .get("content-length")
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    let mut body = buffer[head_end + 4..].to_vec();
    while body.len() < length {
        let read = socket.read(&mut chunk).await.ok()?;
        if read == 0 {
            break;
        }
        body.extend_from_slice(&chunk[..read]);
    }
    Some(Request {
        method,
        path,
        headers,
        body: String::from_utf8_lossy(&body).into_owned(),
    })
}

/// A session object as the API shows it.
pub fn session_json(id: &str, name: &str, state: &str) -> serde_json::Value {
    serde_json::json!({
        "id": id,
        "name": name,
        "owner": "you@example.com",
        "state": state,
        "created": "2026-10-01T12:00:00Z",
        "mcp_url": format!("https://sessions.example.test/{id}/mcp"),
        "policy": {"kind": "rego", "version": 1, "hash": "abc", "state": "ready",
                   "management": {"mode": "editor"}},
    })
}

/// A client for the fake, with fast retries.
pub fn client(fake: &Fake) -> std::sync::Arc<computeruse::Client> {
    computeruse::Client::builder()
        .api_token(TOKEN)
        .base_url(&fake.url)
        .retry_base_delay(std::time::Duration::from_millis(5))
        .build()
        .unwrap()
}
