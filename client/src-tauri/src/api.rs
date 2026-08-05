//! Typed daemon HTTP client (reqwest + rustls). No `tauri-plugin-http`.
//!
//! Every response carries `X-JarJar-Min-Client`; we record a notice whenever the
//! daemon requires a client newer than us (§ API "Versioning & compatibility").

use std::sync::{Arc, Mutex};

use serde::Deserialize;

use crate::state::{Manifest, PackCurrent};

#[derive(Debug, thiserror::Error)]
pub enum ApiError {
    #[error("network error: {0}")]
    Network(String),
    #[error("{code}: {message}")]
    Api {
        status: u16,
        code: String,
        message: String,
    },
    #[error("decode error: {0}")]
    Decode(String),
}

impl ApiError {
    /// Structured error code (`unauthorized`, `not_found`, …) for callers that
    /// branch on it; the UI currently branches on `is_conflict` only.
    #[allow(dead_code)]
    pub fn code(&self) -> &str {
        match self {
            ApiError::Api { code, .. } => code,
            ApiError::Network(_) => "network",
            ApiError::Decode(_) => "decode",
        }
    }
    /// True for the "another request is in flight" case (§ POST /requests).
    pub fn is_conflict(&self) -> bool {
        matches!(self, ApiError::Api { status: 409, .. })
    }
}

#[derive(Debug, Deserialize)]
struct ErrorBody {
    error: ErrorInner,
}
#[derive(Debug, Deserialize)]
struct ErrorInner {
    code: String,
    message: String,
}

// --- Response shapes ---------------------------------------------------------

#[derive(Debug, Clone, Deserialize)]
pub struct JoinResponse {
    pub token: String,
    pub player_id: String,
    #[serde(default)]
    pub role: String,
    pub server_name: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Health {
    pub status: String,
    #[allow(dead_code)]
    pub version: String,
    #[allow(dead_code)]
    pub pack_version: u64,
    #[allow(dead_code)]
    #[serde(default)]
    pub mc_server: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RequestObject {
    pub id: String,
    #[allow(dead_code)]
    #[serde(default)]
    pub player_id: String,
    pub player_name: String,
    pub text: String,
    pub status: String,
    #[allow(dead_code)]
    pub summary: Option<String>,
    pub question: Option<String>,
    pub error: Option<String>,
    pub version: Option<u64>,
    pub created_at: String,
    #[allow(dead_code)]
    pub updated_at: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VersionEntry {
    pub number: u64,
    pub created_at: String,
    pub summary: String,
    #[allow(dead_code)]
    #[serde(default)]
    pub request_id: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Event {
    #[allow(dead_code)]
    pub seq: u64,
    #[serde(rename = "type")]
    pub kind: String,
    #[allow(dead_code)]
    pub created_at: String,
    pub payload: serde_json::Value,
}

#[derive(Debug, Clone, Deserialize)]
pub struct EventsResponse {
    pub events: Vec<Event>,
    pub cursor: u64,
}

// --- Client ------------------------------------------------------------------

#[derive(Clone)]
pub struct DaemonClient {
    client: reqwest::Client,
    base: String,
    token: Option<String>,
    client_version: String,
    /// Set to Some(required_version) when the daemon needs a newer client.
    min_notice: Arc<Mutex<Option<String>>>,
}

impl DaemonClient {
    pub fn new(client: reqwest::Client, base_url: &str, token: Option<String>, version: &str) -> Self {
        DaemonClient {
            client,
            base: base_url.trim_end_matches('/').to_string(),
            token,
            client_version: version.to_string(),
            min_notice: Arc::new(Mutex::new(None)),
        }
    }

    pub fn min_client_notice(&self) -> Option<String> {
        self.min_notice.lock().unwrap().clone()
    }

    fn url(&self, path: &str) -> String {
        format!("{}/api/v1{}", self.base, path)
    }

    fn auth(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        match &self.token {
            Some(t) => rb.bearer_auth(t),
            None => rb,
        }
    }

    /// Inspect headers for X-JarJar-Min-Client and record a notice if we're too old.
    fn check_min_client(&self, headers: &reqwest::header::HeaderMap) {
        if let Some(v) = headers.get("X-JarJar-Min-Client").and_then(|h| h.to_str().ok()) {
            let notice = if semver_gt(v, &self.client_version) {
                Some(format!(
                    "This server requires JarJar {} or newer (you have {}). Please update.",
                    v, self.client_version
                ))
            } else {
                None
            };
            *self.min_notice.lock().unwrap() = notice;
        }
    }

    /// Send a request, map non-2xx into ApiError by parsing the error envelope.
    async fn send(&self, rb: reqwest::RequestBuilder) -> Result<reqwest::Response, ApiError> {
        let resp = rb.send().await.map_err(|e| ApiError::Network(e.to_string()))?;
        self.check_min_client(resp.headers());
        let status = resp.status();
        if status.is_success() {
            return Ok(resp);
        }
        let code_num = status.as_u16();
        // Try the structured error envelope; fall back to a generic code.
        match resp.json::<ErrorBody>().await {
            Ok(b) => Err(ApiError::Api {
                status: code_num,
                code: b.error.code,
                message: b.error.message,
            }),
            Err(_) => Err(ApiError::Api {
                status: code_num,
                code: generic_code(code_num).to_string(),
                message: format!("HTTP {}", code_num),
            }),
        }
    }

    async fn get_json<T: for<'de> Deserialize<'de>>(&self, path: &str) -> Result<T, ApiError> {
        let resp = self.send(self.auth(self.client.get(self.url(path)))).await?;
        resp.json::<T>().await.map_err(|e| ApiError::Decode(e.to_string()))
    }

    // --- Onboarding ---

    pub async fn join(&self, invite_code: &str, player_name: &str) -> Result<JoinResponse, ApiError> {
        let body = serde_json::json!({ "invite_code": invite_code, "player_name": player_name });
        let resp = self
            .send(self.client.post(self.url("/join")).json(&body))
            .await?;
        resp.json().await.map_err(|e| ApiError::Decode(e.to_string()))
    }

    pub async fn health(&self) -> Result<Health, ApiError> {
        self.get_json("/health").await
    }

    // --- Pack & sync ---

    pub async fn pack_current(&self) -> Result<PackCurrent, ApiError> {
        self.get_json("/pack/current").await
    }

    pub async fn manifest(&self, n: u64) -> Result<Manifest, ApiError> {
        self.get_json(&format!("/pack/manifests/{}", n)).await
    }

    /// Fetch a blob, optionally resuming from `range_start`. Returns the streaming
    /// response; the caller verifies SHA-256 while writing.
    pub async fn blob(&self, sha256: &str, range_start: Option<u64>) -> Result<reqwest::Response, ApiError> {
        let mut rb = self.auth(self.client.get(self.url(&format!("/blobs/{}", sha256))));
        if let Some(start) = range_start {
            rb = rb.header(reqwest::header::RANGE, format!("bytes={}-", start));
        }
        self.send(rb).await
    }

    // --- Requests ---

    pub async fn post_request(&self, text: &str) -> Result<RequestObject, ApiError> {
        let body = serde_json::json!({ "text": text });
        let resp = self
            .send(self.auth(self.client.post(self.url("/requests")).json(&body)))
            .await?;
        resp.json().await.map_err(|e| ApiError::Decode(e.to_string()))
    }

    pub async fn answer(&self, id: &str, text: &str) -> Result<RequestObject, ApiError> {
        let body = serde_json::json!({ "text": text });
        let resp = self
            .send(self.auth(self.client.post(self.url(&format!("/requests/{}/answer", id))).json(&body)))
            .await?;
        resp.json().await.map_err(|e| ApiError::Decode(e.to_string()))
    }

    pub async fn list_requests(&self, limit: u32) -> Result<Vec<RequestObject>, ApiError> {
        #[derive(Deserialize)]
        struct Wrap {
            requests: Vec<RequestObject>,
        }
        let w: Wrap = self.get_json(&format!("/requests?limit={}", limit)).await?;
        Ok(w.requests)
    }

    pub async fn list_versions(&self, limit: u32) -> Result<Vec<VersionEntry>, ApiError> {
        #[derive(Deserialize)]
        struct Wrap {
            versions: Vec<VersionEntry>,
        }
        let w: Wrap = self.get_json(&format!("/versions?limit={}", limit)).await?;
        Ok(w.versions)
    }

    // --- Events (long-poll) ---

    pub async fn events(&self, cursor: u64, timeout: u32) -> Result<EventsResponse, ApiError> {
        // Client read timeout must exceed the server's hold window.
        let rb = self
            .auth(self.client.get(self.url(&format!("/events?cursor={}&timeout={}", cursor, timeout))))
            .timeout(std::time::Duration::from_secs((timeout + 10) as u64));
        let resp = self.send(rb).await?;
        resp.json().await.map_err(|e| ApiError::Decode(e.to_string()))
    }
}

fn generic_code(status: u16) -> &'static str {
    match status {
        401 => "unauthorized",
        403 => "forbidden",
        404 => "not_found",
        400 => "invalid_request",
        409 => "conflict",
        429 => "rate_limited",
        _ => "internal",
    }
}

/// Minimal semver "a > b" for X.Y.Z (ignores pre-release/build metadata).
fn semver_gt(a: &str, b: &str) -> bool {
    fn parts(s: &str) -> (u64, u64, u64) {
        let core = s.split(['-', '+']).next().unwrap_or(s);
        let mut it = core.split('.').map(|p| p.parse::<u64>().unwrap_or(0));
        (
            it.next().unwrap_or(0),
            it.next().unwrap_or(0),
            it.next().unwrap_or(0),
        )
    }
    parts(a) > parts(b)
}
