//! One place for what every request shares: the bearer token and its
//! refresh, retries, timeouts, and turning an answer into a value or an
//! error.

use crate::error::bounded;
use crate::types::{ClientOptions, Diagnostic, DEFAULT_BASE_URL};
use crate::ComputerUseError;
use reqwest::header::{HeaderMap, ACCEPT, CONTENT_TYPE, RETRY_AFTER};
use reqwest::{Method, StatusCode};
use serde::de::DeserializeOwned;
use serde::Deserialize;
use std::fmt;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use url::Url;

const DEFAULT_TIMEOUT_MS: u64 = 60_000;
const DEFAULT_MCP_TIMEOUT_MS: u64 = 630_000;
const DEFAULT_MAX_RETRIES: u32 = 3;
const DEFAULT_RETRY_BASE_DELAY_MS: u64 = 500;
const DEFAULT_WAKE_TIMEOUT_MS: u64 = 600_000;
/// No pause between tries is longer than this, whatever `Retry-After` says.
const MAX_RETRY_DELAY: Duration = Duration::from_secs(30);
/// An access token is replaced this long before it expires.
const EXPIRY_MARGIN: Duration = Duration::from_secs(60);
/// The largest body the client reads: `run_js` returns up to 8 MiB of
/// artifacts inline, base64-encoded.
const MAX_BODY_BYTES: usize = 32 << 20;

const SDK_USER_AGENT: &str = concat!("computeruse-sdk/", env!("CARGO_PKG_VERSION"));

/// A string that does not print.
#[derive(Clone)]
pub(crate) struct Secret(String);

impl Secret {
    pub(crate) fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("<redacted>")
    }
}

/// Which failures of a request may be tried again.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Retry {
    /// Doing it twice is doing it once: `429`, `502`, `503`, `504` and a
    /// connection that failed are tried again.
    Idempotent,
    /// A create: only what says the request was not carried out (`429`,
    /// `503`, a connection that could not be made).
    Create,
    /// A session's MCP endpoint: as `Create`, and `504`, which is the
    /// session still waking, is tried again until the wake timeout.
    Mcp,
}

/// A request to make.
pub(crate) struct Call<'a> {
    pub method: Method,
    /// Path below the base URL, with its leading slash.
    pub path: String,
    pub body: Option<Vec<u8>>,
    pub headers: Vec<(&'static str, String)>,
    pub retry: Retry,
    /// What the call is, for a timeout's message.
    pub operation: &'a str,
    /// At least this long is allowed, whatever the client's timeout.
    pub min_timeout: Option<Duration>,
}

impl<'a> Call<'a> {
    pub(crate) fn new(method: Method, path: impl Into<String>, operation: &'a str) -> Self {
        Self {
            method,
            path: path.into(),
            body: None,
            headers: Vec::new(),
            retry: Retry::Idempotent,
            operation,
            min_timeout: None,
        }
    }

    pub(crate) fn json(mut self, body: &serde_json::Value) -> Self {
        self.body = Some(body.to_string().into_bytes());
        self
    }

    pub(crate) fn header(mut self, name: &'static str, value: impl Into<String>) -> Self {
        self.headers.push((name, value.into()));
        self
    }

    pub(crate) fn min_timeout(mut self, timeout: Duration) -> Self {
        self.min_timeout = Some(timeout);
        self
    }

    pub(crate) fn retry(mut self, retry: Retry) -> Self {
        self.retry = retry;
        self
    }
}

/// An answer with a 2xx status.
pub(crate) struct Answer {
    pub headers: HeaderMap,
    pub body: Vec<u8>,
}

impl Answer {
    pub(crate) fn json<T: DeserializeOwned>(&self) -> Result<T, ComputerUseError> {
        serde_json::from_slice(&self.body).map_err(ComputerUseError::decode)
    }

    /// A list, which a server may write as `null` when it is empty.
    pub(crate) fn json_list<T: DeserializeOwned>(&self) -> Result<Vec<T>, ComputerUseError> {
        Ok(self.json::<Option<Vec<T>>>()?.unwrap_or_default())
    }
}

struct AccessToken {
    token: Secret,
    refresh_at: Instant,
}

pub(crate) struct Transport {
    http: reqwest::Client,
    base: Url,
    token: Secret,
    /// The API token's id, when the token is one and is to be exchanged.
    client_id: Option<String>,
    scopes: Vec<String>,
    access: Mutex<Option<AccessToken>>,
    timeout: Duration,
    mcp_timeout: Duration,
    max_retries: u32,
    retry_base_delay: Duration,
    wake_timeout: Duration,
}

impl fmt::Debug for Transport {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Transport")
            .field("base", &self.base.as_str())
            .field("token", &self.token)
            .field("exchange", &self.client_id.is_some())
            .finish_non_exhaustive()
    }
}

impl Transport {
    pub(crate) fn new(options: &ClientOptions) -> Result<Self, ComputerUseError> {
        let token = options.api_token.trim();
        if token.is_empty() {
            return Err(ComputerUseError::configuration("api_token is empty"));
        }
        if token.chars().any(|c| c.is_control() || c.is_whitespace()) {
            return Err(ComputerUseError::configuration(
                "api_token contains whitespace or control characters",
            ));
        }

        let base = parse_base_url(
            options.base_url.as_deref().unwrap_or(DEFAULT_BASE_URL),
            options.allow_insecure_http.unwrap_or(false),
        )?;

        let scopes = options.scopes.clone().unwrap_or_default();
        if scopes
            .iter()
            .any(|s| s.is_empty() || s.contains(char::is_whitespace))
        {
            return Err(ComputerUseError::configuration(
                "a scope is empty or contains whitespace",
            ));
        }

        // Only an API token can be exchanged; an access token is sent as it is.
        let client_id = if options.exchange_token.unwrap_or(true) {
            api_token_id(token)
        } else {
            None
        };
        if client_id.is_none() && !scopes.is_empty() {
            return Err(ComputerUseError::configuration(
                "scopes narrow the access token an API token is exchanged for; \
                 they need exchange_token on and an API token (bjs_...)",
            ));
        }

        let user_agent = match options.user_agent.as_deref().map(str::trim) {
            Some(prefix) if !prefix.is_empty() => format!("{prefix} {SDK_USER_AGENT}"),
            _ => SDK_USER_AGENT.to_owned(),
        };

        let http = reqwest::Client::builder()
            .user_agent(user_agent)
            // The bearer token must not follow a redirect to another host.
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(15))
            .build()
            .map_err(|error| ComputerUseError::configuration(error.to_string()))?;

        Ok(Self {
            http,
            base,
            token: Secret(token.to_owned()),
            client_id,
            scopes,
            access: Mutex::new(None),
            timeout: millis(options.timeout_ms, DEFAULT_TIMEOUT_MS, "timeout_ms")?,
            mcp_timeout: millis(
                options.mcp_timeout_ms,
                DEFAULT_MCP_TIMEOUT_MS,
                "mcp_timeout_ms",
            )?,
            max_retries: options.max_retries.unwrap_or(DEFAULT_MAX_RETRIES),
            retry_base_delay: millis(
                options.retry_base_delay_ms,
                DEFAULT_RETRY_BASE_DELAY_MS,
                "retry_base_delay_ms",
            )?,
            wake_timeout: Duration::from_millis(
                options.wake_timeout_ms.unwrap_or(DEFAULT_WAKE_TIMEOUT_MS),
            ),
        })
    }

    pub(crate) fn base_url(&self) -> &str {
        self.base.as_str().trim_end_matches('/')
    }

    /// The bearer the next request would carry. `force` gets a new access
    /// token even if the cached one has time left.
    pub(crate) async fn bearer(&self, force: bool) -> Result<String, ComputerUseError> {
        let Some(client_id) = &self.client_id else {
            return Ok(self.token.expose().to_owned());
        };
        // Held across the exchange, so that concurrent callers share one.
        let mut cached = self.access.lock().await;
        if !force {
            if let Some(access) = cached.as_ref() {
                if Instant::now() < access.refresh_at {
                    return Ok(access.token.expose().to_owned());
                }
            }
        }
        let access = self.exchange(client_id).await?;
        let token = access.token.expose().to_owned();
        *cached = Some(access);
        Ok(token)
    }

    /// `POST /oauth/token`: the client-credentials grant.
    async fn exchange(&self, client_id: &str) -> Result<AccessToken, ComputerUseError> {
        #[derive(Deserialize)]
        struct Granted {
            access_token: String,
            #[serde(default)]
            expires_in: u64,
        }

        let url = format!("{}/oauth/token", self.base_url());
        let mut form = String::from("grant_type=client_credentials");
        if !self.scopes.is_empty() {
            form.push_str("&scope=");
            form.push_str(
                &url::form_urlencoded::byte_serialize(self.scopes.join(" ").as_bytes())
                    .collect::<String>(),
            );
        }

        let mut attempt = 0;
        loop {
            let sent = self
                .http
                .post(&url)
                .basic_auth(client_id, Some(self.token.expose()))
                .header(CONTENT_TYPE, "application/x-www-form-urlencoded")
                .header(ACCEPT, "application/json")
                .timeout(self.timeout)
                .body(form.clone())
                .send()
                .await;
            let failure = match sent {
                Ok(response) => {
                    let status = response.status();
                    let headers = response.headers().clone();
                    let body = read_body(response, "token exchange").await?;
                    if status.is_success() {
                        let granted: Granted = serde_json::from_slice(&body).map_err(|error| {
                            ComputerUseError::decode(format!("token exchange: {error}"))
                        })?;
                        if granted.access_token.is_empty() {
                            return Err(ComputerUseError::decode(
                                "token exchange: no access_token in the answer",
                            ));
                        }
                        let lifetime = Duration::from_secs(granted.expires_in);
                        // A token of under two minutes is used for half its life.
                        let usable = if lifetime > EXPIRY_MARGIN * 2 {
                            lifetime - EXPIRY_MARGIN
                        } else {
                            lifetime / 2
                        };
                        return Ok(AccessToken {
                            token: Secret(granted.access_token),
                            refresh_at: Instant::now() + usable,
                        });
                    }
                    let retry_after = retry_after(&headers);
                    let error = error_for(status, &headers, &body);
                    if !matches!(status.as_u16(), 429 | 502 | 503 | 504) {
                        return Err(error);
                    }
                    (error, retry_after)
                }
                Err(error) => {
                    let retryable = error.is_connect();
                    let error = transport_error(&error, "token exchange");
                    if !retryable {
                        return Err(error);
                    }
                    (error, None)
                }
            };
            if attempt >= self.max_retries {
                return Err(failure.0);
            }
            tokio::time::sleep(self.backoff(attempt, failure.1)).await;
            attempt += 1;
        }
    }

    /// Sends `call`, with the token, as many times as its retry rule and
    /// the client's limits allow, and returns the first success.
    pub(crate) async fn send(&self, call: Call<'_>) -> Result<Answer, ComputerUseError> {
        let url = format!("{}{}", self.base_url(), call.path);
        let timeout = if call.retry == Retry::Mcp {
            self.mcp_timeout
        } else {
            self.timeout
        };
        let timeout = call.min_timeout.map_or(timeout, |least| timeout.max(least));
        let started = Instant::now();
        let mut attempt = 0;
        let mut refreshed = false;
        let mut force = false;

        loop {
            let bearer = self.bearer(force).await?;
            force = false;

            let mut request = self
                .http
                .request(call.method.clone(), &url)
                .bearer_auth(&bearer)
                .timeout(timeout);
            if !call.headers.iter().any(|(name, _)| *name == "Accept") {
                request = request.header(ACCEPT, "application/json");
            }
            for (name, value) in &call.headers {
                request = request.header(*name, value);
            }
            if let Some(body) = &call.body {
                request = request
                    .header(CONTENT_TYPE, "application/json")
                    .body(body.clone());
            }

            let (error, pause) = match request.send().await {
                Ok(response) => {
                    let status = response.status();
                    let headers = response.headers().clone();
                    let body = read_body(response, call.operation).await?;
                    if status.is_success() {
                        return Ok(Answer { headers, body });
                    }
                    // An access token ends when the backend restarts or its
                    // API token is narrowed: ask once for a new one.
                    if status == StatusCode::UNAUTHORIZED && self.client_id.is_some() && !refreshed
                    {
                        refreshed = true;
                        force = true;
                        continue;
                    }
                    let error = error_for(status, &headers, &body);
                    let waking = call.retry == Retry::Mcp && status == StatusCode::GATEWAY_TIMEOUT;
                    if waking {
                        // Its own budget: a wake can take longer than the
                        // retries of a failing request should.
                        let pause = retry_after(&headers)
                            .unwrap_or(Duration::from_secs(2))
                            .min(Duration::from_secs(10));
                        if started.elapsed() + pause > self.wake_timeout {
                            return Err(ComputerUseError::Timeout {
                                operation: format!(
                                    "{}: the session was still waking after {}s",
                                    call.operation,
                                    self.wake_timeout.as_secs()
                                ),
                            });
                        }
                        tokio::time::sleep(pause).await;
                        continue;
                    }
                    let retryable = match call.retry {
                        Retry::Idempotent => matches!(status.as_u16(), 429 | 502 | 503 | 504),
                        Retry::Create | Retry::Mcp => matches!(status.as_u16(), 429 | 503),
                    };
                    if !retryable {
                        return Err(error);
                    }
                    (error, retry_after(&headers))
                }
                Err(error) => {
                    // A connection that was never made carried no request.
                    let retryable = error.is_connect()
                        || (call.retry == Retry::Idempotent
                            && (error.is_timeout() || error.is_request()));
                    let error = transport_error(&error, call.operation);
                    if !retryable {
                        return Err(error);
                    }
                    (error, None)
                }
            };

            if attempt >= self.max_retries {
                return Err(error);
            }
            tokio::time::sleep(self.backoff(attempt, pause)).await;
            attempt += 1;
        }
    }

    /// The pause before try `attempt + 1`: what the API asked for, or the
    /// base delay doubled each time with up to a quarter added at random.
    fn backoff(&self, attempt: u32, retry_after: Option<Duration>) -> Duration {
        if let Some(asked) = retry_after {
            return asked.min(MAX_RETRY_DELAY);
        }
        let delay = self
            .retry_base_delay
            .saturating_mul(1u32 << attempt.min(16))
            .min(MAX_RETRY_DELAY);
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        delay + delay.mul_f64(f64::from(nanos % 1000) / 4000.0)
    }
}

fn millis(value: Option<u64>, default: u64, name: &str) -> Result<Duration, ComputerUseError> {
    match value {
        Some(0) => Err(ComputerUseError::configuration(format!(
            "{name} must be greater than zero"
        ))),
        Some(value) => Ok(Duration::from_millis(value)),
        None => Ok(Duration::from_millis(default)),
    }
}

/// The API host's URL: https, or http for a loopback address, and no path,
/// query, fragment or credentials.
fn parse_base_url(raw: &str, allow_insecure_http: bool) -> Result<Url, ComputerUseError> {
    let url = Url::parse(raw.trim()).map_err(|error| {
        ComputerUseError::configuration(format!("base_url is not a URL: {error}"))
    })?;
    let loopback = match url.host() {
        Some(url::Host::Domain(domain)) => domain == "localhost",
        Some(url::Host::Ipv4(address)) => address.is_loopback(),
        Some(url::Host::Ipv6(address)) => address.is_loopback(),
        None => {
            return Err(ComputerUseError::configuration("base_url has no host"));
        }
    };
    match url.scheme() {
        "https" => {}
        "http" if loopback || allow_insecure_http => {}
        "http" => {
            return Err(ComputerUseError::configuration(
                "base_url must be https: the token would cross the network in the clear \
                 (http is accepted for localhost only, unless allow_insecure_http is set)",
            ));
        }
        other => {
            return Err(ComputerUseError::configuration(format!(
                "base_url must be an https URL, not {other}"
            )));
        }
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err(ComputerUseError::configuration(
            "base_url must not carry credentials",
        ));
    }
    if !url.path().trim_matches('/').is_empty() || url.query().is_some() || url.fragment().is_some()
    {
        return Err(ComputerUseError::configuration(
            "base_url is the API host alone, such as https://api.computeruse.site: \
             no path (not /v1), query or fragment",
        ));
    }
    Ok(url)
}

/// The id of an API token `bjs_<id>_<secret>`: 12 characters of lower-case
/// base32. `None` for anything else, such as an access token.
pub(crate) fn api_token_id(token: &str) -> Option<String> {
    let rest = token.strip_prefix("bjs_")?;
    let (id, secret) = rest.split_once('_')?;
    let well_formed = id.len() == 12
        && id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || (b'2'..=b'7').contains(&b))
        && !secret.is_empty();
    well_formed.then(|| id.to_owned())
}

async fn read_body(
    mut response: reqwest::Response,
    operation: &str,
) -> Result<Vec<u8>, ComputerUseError> {
    let mut body = Vec::new();
    loop {
        match response.chunk().await {
            Ok(Some(chunk)) => {
                if body.len() + chunk.len() > MAX_BODY_BYTES {
                    return Err(ComputerUseError::Decode {
                        reason: format!("{operation}: the answer is larger than 32 MiB"),
                    });
                }
                body.extend_from_slice(&chunk);
            }
            Ok(None) => return Ok(body),
            Err(error) => return Err(transport_error(&error, operation)),
        }
    }
}

/// reqwest's error without its URL: a URL is not a secret here, but the
/// message is shorter and the same in every binding.
fn transport_error(error: &reqwest::Error, operation: &str) -> ComputerUseError {
    if error.is_timeout() {
        return ComputerUseError::Timeout {
            operation: operation.to_owned(),
        };
    }
    let mut reason = error.to_string();
    let mut source = std::error::Error::source(error);
    while let Some(inner) = source {
        reason.push_str(": ");
        reason.push_str(&inner.to_string());
        source = inner.source();
    }
    ComputerUseError::Transport {
        reason: format!("{operation}: {reason}"),
    }
}

fn retry_after(headers: &HeaderMap) -> Option<Duration> {
    let seconds: u64 = headers
        .get(RETRY_AFTER)?
        .to_str()
        .ok()?
        .trim()
        .parse()
        .ok()?;
    Some(Duration::from_secs(seconds))
}

/// The error an answer that is not a success stands for. The API's errors
/// are `{"error": "<sentence>", ...}`; the session proxy's are plain text;
/// the MCP endpoint's billing refusal is a JSON-RPC error.
pub(crate) fn error_for(status: StatusCode, headers: &HeaderMap, body: &[u8]) -> ComputerUseError {
    #[derive(Deserialize, Default)]
    struct Body {
        #[serde(default)]
        error: serde_json::Value,
        #[serde(default)]
        error_description: Option<String>,
        #[serde(default)]
        code: Option<String>,
        #[serde(default, rename = "billingUrl")]
        billing_url: Option<String>,
        #[serde(default)]
        managed_url: Option<String>,
        #[serde(default, deserialize_with = "crate::types::null_as_empty")]
        errors: Vec<Diagnostic>,
        #[serde(default, deserialize_with = "crate::types::null_as_empty")]
        warnings: Vec<Diagnostic>,
    }

    let text = String::from_utf8_lossy(body);
    let parsed: Body = serde_json::from_slice(body).unwrap_or_default();
    let message = match &parsed.error {
        serde_json::Value::String(sentence) => sentence.clone(),
        // JSON-RPC: {"error": {"code": ..., "message": "..."}}.
        serde_json::Value::Object(rpc) => rpc
            .get("message")
            .and_then(|m| m.as_str())
            .unwrap_or_default()
            .to_owned(),
        _ => String::new(),
    };
    let message = match (message.is_empty(), parsed.error_description) {
        (false, Some(description)) => format!("{message}: {description}"),
        (false, None) => message,
        (true, _) if !text.trim().is_empty() => bounded(&text),
        (true, _) => status.canonical_reason().unwrap_or("no message").to_owned(),
    };
    let message = bounded(&message);
    let nonempty = |value: Option<String>| value.filter(|v| !v.is_empty());

    let code = nonempty(parsed.code);
    let billing_url = nonempty(parsed.billing_url);
    match status.as_u16() {
        401 => ComputerUseError::Unauthorized { message },
        402 => ComputerUseError::PaymentRequired {
            message,
            code,
            billing_url,
        },
        403 => ComputerUseError::Forbidden {
            message,
            code,
            billing_url,
        },
        404 => ComputerUseError::NotFound { message },
        409 => ComputerUseError::Conflict {
            message,
            code,
            managed_url: nonempty(parsed.managed_url),
            billing_url,
        },
        422 => ComputerUseError::InvalidPolicy {
            message,
            errors: parsed.errors,
            warnings: parsed.warnings,
        },
        429 => ComputerUseError::RateLimited {
            message,
            retry_after_secs: retry_after(headers).map(|d| d.as_secs()),
        },
        status => ComputerUseError::Api {
            status,
            message,
            code,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn api_token_ids() {
        assert_eq!(
            api_token_id("bjs_k3xw5qj2m7ab_c2VjcmV0X3dpdGhfdW5kZXJzY29yZXM"),
            Some("k3xw5qj2m7ab".into())
        );
        // A secret may contain underscores.
        assert_eq!(
            api_token_id("bjs_k3xw5qj2m7ab_a_b_c"),
            Some("k3xw5qj2m7ab".into())
        );
        assert_eq!(api_token_id("eyJhbGciOiJIUzI1NiJ9.e30.x"), None);
        assert_eq!(api_token_id("bjs_SHOUTING1234_secret"), None);
        assert_eq!(api_token_id("bjs_short_secret"), None);
        assert_eq!(api_token_id("bjs_k3xw5qj2m7ab_"), None);
    }

    #[test]
    fn base_urls() {
        assert!(parse_base_url("https://api.computeruse.site", false).is_ok());
        assert!(parse_base_url("https://api.computeruse.site/", false).is_ok());
        assert!(parse_base_url("http://127.0.0.1:8080", false).is_ok());
        assert!(parse_base_url("http://localhost:8080", false).is_ok());
        assert!(parse_base_url("http://[::1]:8080", false).is_ok());
        assert!(parse_base_url("http://api.internal.test", true).is_ok());
        assert!(parse_base_url("ftp://api.internal.test", true).is_err());
        for bad in [
            "http://api.computeruse.site",
            "https://api.computeruse.site/v1",
            "https://user:pw@api.computeruse.site",
            "https://api.computeruse.site?x=1",
            "ftp://api.computeruse.site",
            "api.computeruse.site",
        ] {
            assert!(
                matches!(
                    parse_base_url(bad, false),
                    Err(ComputerUseError::Configuration { .. })
                ),
                "{bad}"
            );
        }
    }

    #[test]
    fn errors_from_answers() {
        let none = HeaderMap::new();
        let e = error_for(
            StatusCode::PAYMENT_REQUIRED,
            &none,
            br#"{"error":"Add credit.","code":"out_of_credit","billingUrl":"https://app.example/billing"}"#,
        );
        assert_eq!(
            e,
            ComputerUseError::PaymentRequired {
                message: "Add credit.".into(),
                code: Some("out_of_credit".into()),
                billing_url: Some("https://app.example/billing".into()),
            }
        );
        // The proxy's plain text.
        let e = error_for(StatusCode::CONFLICT, &none, b"session is stopped\n");
        assert_eq!(
            e,
            ComputerUseError::Conflict {
                message: "session is stopped".into(),
                code: None,
                managed_url: None,
                billing_url: None
            }
        );
        // The MCP endpoint's billing refusal.
        let e = error_for(
            StatusCode::PAYMENT_REQUIRED,
            &none,
            br#"{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":"out of credit"}}"#,
        );
        assert!(
            matches!(e, ComputerUseError::PaymentRequired { ref message, .. } if message == "out of credit")
        );
        // No body at all.
        let e = error_for(StatusCode::BAD_GATEWAY, &none, b"");
        assert_eq!(e.status(), Some(502));
        assert!(e.to_string().contains("Bad Gateway"));
    }
}
