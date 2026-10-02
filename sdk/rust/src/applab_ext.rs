//! The parts of the AppLab client a code generator cannot write.
//!
//! This is the Rust twin of `sdk/python/applab_ext.py` and
//! `sdk/typescript/src/applabExt.ts`. openapi-generator produces a method per
//! endpoint from `api/openapi.yaml`, which covers most of the API, but four
//! things are not one request:
//!
//!   * A chunked upload. The server needs the part count *before* the first part
//!     arrives, so the archive has to be on disk first, and the part size comes
//!     from a separate call to `/api/v1/config`. A generated one-request method
//!     for `POST /source` cannot do that.
//!   * Following a log. A generated method reads a response to its end, which for
//!     `follow=true` is a stream that may never end — it would hang.
//!   * Packing a directory. That is a client concern, not an API operation.
//!   * Resolving an abbreviated build id, which is a lookup plus a conflict check
//!     rather than a single call.
//!
//! This file is hand-written and is listed in `.openapi-generator-ignore`, so
//! regeneration never touches it. It talks to the same API as the generated
//! client and takes the same two things — an address and a key — rather than
//! depending on the generated client's own types, so a generator upgrade cannot
//! silently break it.
//!
//! # Dependencies
//!
//! Rust's standard library has neither an HTTP client nor a tar writer, so unlike
//! the Python and TypeScript helpers this one cannot be dependency-free — that is
//! the language's doing, not a choice.
//!
//! The tar writer, though, is `tar` the *format*, not the crate: it is eighty
//! lines of fixed-width octal fields and is written out below, the same way the
//! Java helper writes it. That is deliberate. `.openapi-generator-ignore` cannot
//! protect `Cargo.toml` — the generator rewrites it whenever the API changes — so
//! any crate this file needed would have to be re-added by hand after every
//! regeneration, and a forgotten line would surface as a compile error in CI
//! rather than as anything a reader could connect to the API change. Writing the
//! format out removes the whole class of problem.
//!
//! The archive is an *uncompressed* tar. The server reads the gzip magic off the
//! first two bytes and decompresses only if it is there (see
//! `internal/source/ingest.go`), so plain tar is accepted, and skipping gzip
//! drops the `flate2` dependency as well. `reqwest`, `serde`, `serde_json`,
//! `tokio` and `futures-util` all come with the generated crate; `stream` on
//! reqwest is not optional here, because it is what `Response::bytes_stream` —
//! the incremental read that keeps a followed log from hanging — is behind.
//!
//! If the specification ever changes the generated crate's dependencies, this
//! file is the place that will notice: it names the ones it relies on rather than
//! assuming them.
//!
//! The client is the caller's to build, so its timeouts and TLS settings stay the
//! application's to choose:
//!
//! ```text
//! let client = reqwest::Client::new();
//! let (url, key) = ("https://applab.example.com/applab", "…");
//!
//! let opts = applab_ext::UploadOptions::default().with_message("from rust");
//! applab_ext::push_directory(&client, url, key, "shop", Path::new("./shop"), &opts).await?;
//!
//! let mut lines = applab_ext::stream_logs(&client, url, key, "shop", &applab_ext::LogOptions::following()).await?;
//! while let Some(line) = lines.next().await {
//!     print!("{}", line?);
//! }
//! ```
//!
//! The module path those calls use depends on how the generated `lib.rs` exposes
//! this file, which is the generator's to decide — check it once the tree has been
//! generated. The snippets here are illustrative rather than doctests for that
//! reason: this file should not make `cargo test` depend on a path it does not own.

use std::fmt;
use std::fs;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::task::{Context, Poll};
use std::time::{SystemTime, UNIX_EPOCH};

use futures_util::{Stream, StreamExt};
use reqwest::{Client, RequestBuilder, StatusCode};
use serde::Deserialize;
use serde_json::Value;
use tokio::io::AsyncReadExt;

/// The fallback part size, used only when the deployment does not report one. It
/// matches the server's own default (8 MiB).
pub const DEFAULT_CHUNK_SIZE: u64 = 8 * 1024 * 1024;

/// Directories never included in a push. The same set the CLI, the Go client and
/// the console use. It is repeated here rather than fetched because it is a
/// client-side decision about what a person means by "this directory", not
/// something the server can know.
pub const SKIP_DIRS: &[&str] = &[
    ".git", "node_modules", "target", "dist", "build", ".venv", "venv",
    "__pycache__", ".next", ".nuxt", "vendor", ".idea", ".vscode", ".DS_Store",
];

/// The name field of a tar header is 100 bytes; a longer path is refused. The tar
/// crate would write a GNU extension header for it, but the other SDKs refuse
/// instead, and an archive whose shape depends on which client packed it is a
/// difference nobody would think to look for.
const TAR_NAME_MAX: usize = 100;

/// A failure the deployment reported, or a local one that stopped the call.
#[derive(Debug)]
pub struct AppLabError {
    /// The HTTP status, or 0 when the deployment could not be reached at all.
    pub status: u16,
    /// What went wrong, as the deployment worded it where there was one.
    pub message: String,
    /// Whether the server considers the same call worth retrying.
    pub retryable: bool,
}

impl AppLabError {
    /// Build an error from a status the deployment returned.
    pub fn new(status: u16, message: impl Into<String>, retryable: bool) -> Self {
        Self { status, message: message.into(), retryable }
    }

    /// A transport failure — the deployment could not be reached at all.
    ///
    /// It is the only case where there is no status to report, and it is retryable
    /// in the sense that the same call may well work once the network does.
    fn transport(err: reqwest::Error) -> Self {
        Self::new(0, format!("cannot reach the deployment: {err}"), true)
    }

    /// A failure on this machine: reading the tree, writing the archive.
    fn local(message: impl Into<String>) -> Self {
        Self::new(0, message, false)
    }
}

impl fmt::Display for AppLabError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.message)
    }
}

impl std::error::Error for AppLabError {}

impl From<reqwest::Error> for AppLabError {
    fn from(err: reqwest::Error) -> Self {
        AppLabError::transport(err)
    }
}

impl From<io::Error> for AppLabError {
    fn from(err: io::Error) -> Self {
        AppLabError::local(format!("local failure: {err}"))
    }
}

impl From<serde_json::Error> for AppLabError {
    fn from(err: serde_json::Error) -> Self {
        AppLabError::local(format!("the deployment answered something that is not JSON: {err}"))
    }
}

/// The deployment's self-description, as far as the helpers need it.
#[derive(Debug, Clone, Copy)]
pub struct Config {
    /// The part size the deployment wants, in bytes.
    pub chunk_size: u64,
    /// The largest archive it accepts in one request, or 0 when it does not say.
    pub max_simple_upload: u64,
    /// The largest single part it accepts, or 0 when it does not say.
    pub max_chunk_bytes: u64,
}

/// What a source upload produced.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct UploadResult {
    #[serde(default)]
    pub branch: Option<String>,
    #[serde(default)]
    pub commit_sha: Option<String>,
    #[serde(default)]
    pub message: Option<String>,
    #[serde(default)]
    pub files: Option<u64>,
    #[serde(default)]
    pub bytes: Option<u64>,
    #[serde(default)]
    pub stripped_root: Option<String>,
}

/// What a caller may say about an upload.
#[derive(Debug, Clone)]
pub struct UploadOptions {
    /// The commit message.
    pub message: Option<String>,
    /// The branch to store the commit on. `None` means the app's default.
    pub branch: Option<String>,
    /// Whether the server should build and deploy the commit it stores. It does
    /// so when this is left at its default of `true`; see [`UploadOptions::publish`]
    /// for why a caller would ever turn it off.
    pub publish: bool,
    /// The part size to use, overriding the deployment's advertised one.
    pub chunk_size: Option<u64>,
}

impl Default for UploadOptions {
    fn default() -> Self {
        // publish defaults to true, which is why this is written out rather than
        // derived: the derived default would be false, and an upload that quietly
        // stopped deploying things would be a hard bug to see.
        Self { message: None, branch: None, publish: true, chunk_size: None }
    }
}

impl UploadOptions {
    /// Set the commit message.
    pub fn with_message(mut self, message: impl Into<String>) -> Self {
        self.message = Some(message.into());
        self
    }

    /// Store the commit on a branch other than the app's default.
    pub fn with_branch(mut self, branch: impl Into<String>) -> Self {
        self.branch = Some(branch.into());
        self
    }

    /// Ask the server not to build and deploy what it stores.
    ///
    /// Set this false only when the caller ships the commit itself — the CLI
    /// uploads and then calls the build and deploy endpoints by name, so an upload
    /// that also published would be built twice and the caller would be watching a
    /// build they did not start. Leaving it alone is what makes changing an app's
    /// source over the API mean what it means over git.
    pub fn publish(mut self, publish: bool) -> Self {
        self.publish = publish;
        self
    }

    /// Override the part size the deployment advertises.
    pub fn with_chunk_size(mut self, chunk_size: u64) -> Self {
        self.chunk_size = Some(chunk_size);
        self
    }
}

/// What a caller may say about a log stream.
#[derive(Debug, Clone, Default)]
pub struct LogOptions {
    /// Keep the response open as new lines arrive. This is the whole reason the
    /// helper exists: a plain method would buffer a response that may never end.
    pub follow: bool,
    pub pod: Option<String>,
    pub container: Option<String>,
    pub tail: Option<u64>,
    /// Read the previous container's logs, for a pod that has restarted.
    pub previous: bool,
    /// An RFC 3339 timestamp or a duration such as `5m`.
    pub since: Option<String>,
}

impl LogOptions {
    /// The default: existing lines, then stop.
    pub fn once() -> Self {
        Self::default()
    }

    /// Keep streaming after the existing lines.
    pub fn following() -> Self {
        Self { follow: true, ..Self::default() }
    }

    /// Keep the response open as new lines arrive.
    pub fn follow(mut self, follow: bool) -> Self {
        self.follow = follow;
        self
    }

    /// Read one pod rather than the app's whole set.
    pub fn pod(mut self, pod: impl Into<String>) -> Self {
        self.pod = Some(pod.into());
        self
    }

    /// Read one container of the pod.
    pub fn container(mut self, container: impl Into<String>) -> Self {
        self.container = Some(container.into());
        self
    }

    /// Start from the last `tail` lines rather than the beginning.
    pub fn tail(mut self, tail: u64) -> Self {
        self.tail = Some(tail);
        self
    }

    /// Read the previous container's logs.
    pub fn previous(mut self, previous: bool) -> Self {
        self.previous = previous;
        self
    }

    /// Start from a timestamp or a duration such as `5m`.
    pub fn since(mut self, since: impl Into<String>) -> Self {
        self.since = Some(since.into());
        self
    }
}

/// An app's log lines, one at a time.
///
/// A [`Stream`] rather than a `String`, because with `follow` the response does
/// not end until the app stops logging — reading it to its end would never
/// return. It is unpinned, so it can be held in a struct or a `select!` arm.
///
/// Consuming it needs `futures_util::StreamExt` in scope; that is already a
/// dependency of the generated crate.
///
/// ```text
/// let mut lines = applab_ext::stream_logs(&client, url, key, "shop", &LogOptions::following()).await?;
/// while let Some(line) = lines.next().await {
///     print!("{}", line?);
/// }
/// ```
///
/// Each item is `Result<String, AppLabError>`, so a transport failure part-way
/// through a followed log is reported to the consumer rather than ending the
/// stream silently. Dropping the value closes the response, and with it the
/// connection — which is how a follower stops.
pub struct Lines {
    // Boxed and pinned because the concrete type of reqwest's body stream is
    // unnameable (`impl Stream`), and boxed rather than generic because this type
    // is part of the module's API and a generic parameter would push that
    // unnameable type onto every caller.
    inner: Pin<Box<dyn Stream<Item = reqwest::Result<Vec<u8>>> + Send>>,
    buffer: Vec<u8>,
    finished: bool,
}

impl Lines {
    fn new(response: reqwest::Response) -> Self {
        // `bytes_stream` is read chunk by chunk as it arrives. `text()` or
        // `bytes()` would wait for the end of a stream that, with `follow`, may
        // never have one — the hang this whole helper exists to avoid.
        let inner = response.bytes_stream().map(|chunk| chunk.map(|bytes| bytes.to_vec()));
        Self { inner: Box::pin(inner), buffer: Vec::new(), finished: false }
    }

    /// Take one complete line out of the buffer, if it holds one.
    ///
    /// Lines keep their terminator, matching the other SDKs: a caller printing
    /// what it is handed does not have to put newlines back.
    fn take_line(&mut self) -> Option<String> {
        let end = self.buffer.iter().position(|&b| b == b'\n')?;
        let line: Vec<u8> = self.buffer.drain(..=end).collect();
        Some(String::from_utf8_lossy(&line).into_owned())
    }
}

impl Stream for Lines {
    type Item = Result<String, AppLabError>;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        let this = self.get_mut();
        loop {
            if let Some(line) = this.take_line() {
                return Poll::Ready(Some(Ok(line)));
            }
            if this.finished {
                // A last line without a terminator is still a line.
                if this.buffer.is_empty() {
                    return Poll::Ready(None);
                }
                let line = String::from_utf8_lossy(&this.buffer).into_owned();
                this.buffer.clear();
                return Poll::Ready(Some(Ok(line)));
            }
            match this.inner.as_mut().poll_next(cx) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(None) => this.finished = true,
                Poll::Ready(Some(Ok(chunk))) => this.buffer.extend_from_slice(&chunk),
                Poll::Ready(Some(Err(err))) => {
                    this.finished = true;
                    return Poll::Ready(Some(Err(AppLabError::transport(err))));
                }
            }
        }
    }
}

// --- the helpers the generator cannot write --------------------------------

/// Read the deployment's limits, which the chunked upload needs.
///
/// A limit the deployment does not report comes back as 0, meaning "no limit
/// known" — the helpers then trust the deployment to refuse rather than inventing
/// a ceiling of their own. `chunk_size` is the exception: it falls back to the
/// server's own default, because a part size of zero would divide by it.
pub async fn get_config(client: &Client, base_url: &str, key: &str) -> Result<Config, AppLabError> {
    let response = client.get(endpoint(base_url, "/api/v1/config")).bearer_auth(key).send().await?;
    let data = unwrap(response).await?;
    Ok(Config {
        chunk_size: data.get("chunk_size").and_then(Value::as_u64).filter(|n| *n > 0).unwrap_or(DEFAULT_CHUNK_SIZE),
        max_simple_upload: data.get("max_simple_upload").and_then(Value::as_u64).unwrap_or(0),
        max_chunk_bytes: data.get("max_chunk_bytes").and_then(Value::as_u64).unwrap_or(0),
    })
}

/// Tar a directory into a temporary file, returning its path.
///
/// The result is deterministic — entries are walked in sorted order, names are
/// relative and forward-slashed, and mtimes are preserved — so the same tree
/// produces the same archive and a re-push of an unchanged directory is a no-op
/// commit rather than a diff of timestamps.
///
/// The archive is streamed straight to a file rather than assembled in memory:
/// the chunked upload exists for trees too large for one request, and packing one
/// of those into a `Vec` would be the moment the client needed the most memory —
/// exactly backwards. It is an uncompressed tar, which the server accepts; see
/// the module comment for why gzip is left out.
///
/// The caller owns the file and should delete it. [`push_directory`] is the
/// version that does both.
pub async fn package_directory(directory: &Path) -> Result<PathBuf, AppLabError> {
    let root = directory.to_path_buf();
    let path = temp_archive_path();

    // The walk touches the filesystem with blocking calls, so it runs off the
    // async executor. On a source tree of any size this is not a microsecond of
    // work, and a blocking walk inside a `tokio::spawn`'d task is how one push
    // stalls the whole runtime.
    let packed = tokio::task::spawn_blocking({
        let root = root.clone();
        let path = path.clone();
        move || pack(&root, &path)
    })
    .await
    .map_err(|err| AppLabError::local(format!("packing the directory did not finish: {err}")))?;

    if let Err(err) = packed {
        let _ = fs::remove_file(&path);
        return Err(err);
    }
    Ok(path)
}

/// Package a directory and upload it as a commit. This is the `applab push` path.
///
/// It always uses the chunked upload, even for a small tree, because the chunked
/// path has no size limit and choosing between two paths on a size the server has
/// not been asked about yet is how a push fails at exactly the wrong moment.
pub async fn push_directory(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    directory: &Path,
    opts: &UploadOptions,
) -> Result<UploadResult, AppLabError> {
    let archive = package_directory(directory).await?;
    let result = upload_source_chunked(client, base_url, key, app_id, &archive, opts).await;
    // Removed whatever happened: the staged archive is a copy of a tree the
    // caller still has, so keeping it on failure would fill the disk with the
    // largest thing this client ever writes.
    let _ = fs::remove_file(&archive);
    result
}

/// Upload an archive in one request.
///
/// For a tree under the deployment's own limit this is one call instead of the
/// several a chunked upload makes. Over the limit the server answers 413 and
/// names the chunked endpoints; that is turned into a call to
/// [`upload_source_chunked`], so a caller that guessed "small enough" is not left
/// to handle it.
pub async fn upload_source(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    archive_path: &Path,
    opts: &UploadOptions,
) -> Result<UploadResult, AppLabError> {
    let path = format!("/api/v1/apps/{app_id}/source");
    // A File rather than a Vec: the body streams from disk, so even this
    // "one request" path does not hold the archive in memory.
    let response = with_query(client.post(endpoint(base_url, &path)), &upload_query(opts))
        .bearer_auth(key)
        .header(reqwest::header::CONTENT_TYPE, "application/x-tar")
        .body(fs::File::open(archive_path)?)
        .send()
        .await?;

    if response.status() == StatusCode::PAYLOAD_TOO_LARGE {
        return upload_source_chunked(client, base_url, key, app_id, archive_path, opts).await;
    }
    unwrap_into(response).await
}

/// Upload an archive in parts and return the resulting commit.
///
/// The archive must already exist as a file: the part count has to be known
/// before the first part is sent, which cannot be done from a stream without
/// staging it somewhere first. This is a requirement of the protocol rather than
/// a convenience — and it is why [`push_directory`] packs to a temporary file
/// before calling this.
pub async fn upload_source_chunked(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    archive_path: &Path,
    opts: &UploadOptions,
) -> Result<UploadResult, AppLabError> {
    let size = fs::metadata(archive_path)?.len();
    let config = get_config(client, base_url, key).await?;
    let part_size = opts.chunk_size.filter(|n| *n > 0).unwrap_or(config.chunk_size);
    if config.max_chunk_bytes > 0 && part_size > config.max_chunk_bytes {
        return Err(AppLabError::local(format!(
            "chunk size {part_size} exceeds the deployment's limit of {}",
            config.max_chunk_bytes
        )));
    }
    // Rounded up, so a size that is not a multiple of the part size still gets a
    // final short part. Written out rather than using `div_ceil` because that
    // stabilised recently and this file should not raise the crate's minimum
    // toolchain for one line.
    let total = std::cmp::max(1, (size + part_size - 1) / part_size);

    let started = unwrap(
        client
            .post(endpoint(base_url, &format!("/api/v1/apps/{app_id}/source/uploads")))
            .bearer_auth(key)
            .json(&serde_json::json!({
                "total": total,
                "chunk_size": part_size,
                "message": opts.message.clone().unwrap_or_default(),
            }))
            .send()
            .await?,
    )
    .await?;

    let upload_id = started.get("upload_id").and_then(Value::as_str).unwrap_or_default().to_string();
    if upload_id.is_empty() {
        return Err(AppLabError::local("the deployment did not return an upload id"));
    }

    let parts = endpoint(base_url, &format!("/api/v1/apps/{app_id}/source/uploads/{upload_id}/parts"));
    let mut file = tokio::fs::File::open(archive_path).await?;
    let mut buffer = vec![0u8; part_size as usize];
    for index in 1..=total {
        let offset = (index - 1) * part_size;
        let want = std::cmp::min(part_size, size - offset) as usize;
        file.read_exact(&mut buffer[..want]).await?;
        // The index is 1-based and is which part this is, not a byte offset; the
        // server rejects a gap.
        unwrap(
            client
                .put(format!("{parts}/{index}"))
                .bearer_auth(key)
                .header(reqwest::header::CONTENT_TYPE, "application/octet-stream")
                .body(buffer[..want].to_vec())
                .send()
                .await?,
        )
        .await?;
    }

    // `/complete` carries the message and branch rather than the body, and
    // `publish=false` is the only publish value that ever travels.
    let complete = format!("/api/v1/apps/{app_id}/source/uploads/{upload_id}/complete");
    unwrap_into(
        with_query(client.post(endpoint(base_url, &complete)), &upload_query(opts))
            .bearer_auth(key)
            .send()
            .await?,
    )
    .await
}

/// An app's log lines, one at a time.
///
/// Returns a [`Lines`] stream rather than a string, because with
/// `follow: true` the response does not end until the app stops logging and
/// reading it into a string would never return. Iterating lets a caller print
/// each line as it arrives, which is the whole point of following.
///
/// The response is read incrementally; the connection stays open until the
/// stream is dropped.
pub async fn stream_logs(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    opts: &LogOptions,
) -> Result<Lines, AppLabError> {
    let mut query: Vec<(&str, String)> = Vec::new();
    if opts.follow {
        query.push(("follow", "true".to_string()));
    }
    if let Some(pod) = &opts.pod {
        query.push(("pod", pod.clone()));
    }
    if let Some(container) = &opts.container {
        query.push(("container", container.clone()));
    }
    if let Some(tail) = opts.tail {
        query.push(("tail", tail.to_string()));
    }
    if opts.previous {
        query.push(("previous", "true".to_string()));
    }
    if let Some(since) = &opts.since {
        query.push(("since", since.clone()));
    }

    let path = format!("/api/v1/apps/{app_id}/logs");
    let response = with_query(client.get(endpoint(base_url, &path)), &query)
        .bearer_auth(key)
        .send()
        .await?;
    if !response.status().is_success() {
        return Err(error_from(response).await);
    }
    Ok(Lines::new(response))
}

/// A build's log lines. The build variant of [`stream_logs`].
pub async fn stream_build_logs(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    build_id: &str,
    follow: bool,
) -> Result<Lines, AppLabError> {
    let path = format!("/api/v1/apps/{app_id}/builds/{build_id}/logs");
    let query: Vec<(&str, &str)> = if follow { vec![("follow", "true")] } else { Vec::new() };
    let response = with_query_str(client.get(endpoint(base_url, &path)), &query)
        .bearer_auth(key)
        .send()
        .await?;
    if !response.status().is_success() {
        return Err(error_from(response).await);
    }
    Ok(Lines::new(response))
}

/// Find the build whose id starts with `prefix`.
///
/// Builds are shown abbreviated and typed abbreviated. This is what turns what a
/// person copied off a screen into the id the API wants, and it is deliberately
/// strict: two builds sharing a prefix is an error rather than a coin flip,
/// because guessing is how the wrong build gets cancelled.
pub async fn resolve_build_id(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    prefix: &str,
) -> Result<String, AppLabError> {
    resolve_build_id_limited(client, base_url, key, app_id, prefix, 50).await
}

/// [`resolve_build_id`] with an explicit page size.
pub async fn resolve_build_id_limited(
    client: &Client,
    base_url: &str,
    key: &str,
    app_id: &str,
    prefix: &str,
    limit: u64,
) -> Result<String, AppLabError> {
    let path = format!("/api/v1/apps/{app_id}/builds");
    let query: Vec<(&str, String)> = vec![("limit", limit.to_string())];
    let builds = unwrap(
        with_query(client.get(endpoint(base_url, &path)), &query)
            .bearer_auth(key)
            .send()
            .await?,
    )
    .await?;

    let matches: Vec<&str> = builds
        .as_array()
        .map(|items| {
            items
                .iter()
                .filter_map(|item| item.get("id").and_then(Value::as_str))
                .filter(|id| id.starts_with(prefix))
                .collect()
        })
        .unwrap_or_default();

    match matches.len() {
        0 => Err(AppLabError::new(404, format!("no build of {app_id} starts with {prefix}"), false)),
        1 => Ok(matches[0].to_string()),
        n => Err(AppLabError::new(409, format!("{prefix} matches {n} builds; be more specific"), false)),
    }
}

// --- requests and the envelope ---------------------------------------------

/// Join an address and a path without doubling or losing the slash.
fn endpoint(base_url: &str, path: &str) -> String {
    format!("{}{}", base_url.trim_end_matches('/'), path)
}

/// Attach a query string, or leave the request alone when there is nothing to
/// say.
///
/// An empty `query` is not harmless: `reqwest` would still end the URL with a
/// bare `?`, which some proxies treat as a different path.
fn with_query(request: RequestBuilder, query: &[(&str, String)]) -> RequestBuilder {
    if query.is_empty() {
        request
    } else {
        request.query(query)
    }
}

/// [`with_query`] for a borrowed, static query — the one-value case.
fn with_query_str(request: RequestBuilder, query: &[(&str, &str)]) -> RequestBuilder {
    if query.is_empty() {
        request
    } else {
        request.query(query)
    }
}

/// The query an upload's query parameters describe.
///
/// `publish=false` is the only publish value that ever travels — the server
/// publishes when the parameter is absent, so sending `publish=true` would be
/// saying the default out loud and would stop being the default the day it
/// changed.
fn upload_query(opts: &UploadOptions) -> Vec<(&'static str, String)> {
    let mut query: Vec<(&'static str, String)> = Vec::new();
    if let Some(message) = &opts.message {
        query.push(("message", message.clone()));
    }
    if let Some(branch) = &opts.branch {
        query.push(("branch", branch.clone()));
    }
    if !opts.publish {
        query.push(("publish", "false".to_string()));
    }
    query
}

/// Unwrap the `{"data": ...}` envelope every ordinary route answers with.
async fn unwrap(response: reqwest::Response) -> Result<Value, AppLabError> {
    if !response.status().is_success() {
        return Err(error_from(response).await);
    }
    let text = response.text().await?;
    if text.trim().is_empty() {
        return Ok(Value::Null);
    }
    let value: Value = serde_json::from_str(&text)?;
    Ok(match value {
        Value::Object(mut map) if map.contains_key("data") => map.remove("data").unwrap_or(Value::Null),
        other => other,
    })
}

/// [`unwrap`] into a typed result.
async fn unwrap_into<T: for<'de> Deserialize<'de>>(response: reqwest::Response) -> Result<T, AppLabError> {
    let data = unwrap(response).await?;
    serde_json::from_value(data).map_err(AppLabError::from)
}

/// Turn an HTTP failure into an [`AppLabError`], reading the error envelope.
///
/// A body that is not JSON — a proxy's HTML, a 413 from something in front of the
/// deployment — is reported by status and an excerpt, which is more useful than a
/// parse error from a document that was never the API's.
async fn error_from(response: reqwest::Response) -> AppLabError {
    let status = response.status();
    let text = response.text().await.unwrap_or_default();
    if let Ok(value) = serde_json::from_str::<Value>(&text) {
        if let Some(message) = value.get("error").and_then(Value::as_str) {
            let retryable = value.get("retryable").and_then(Value::as_bool).unwrap_or(false);
            return AppLabError::new(status.as_u16(), message, retryable);
        }
    }
    let excerpt: String = text.chars().take(200).collect();
    AppLabError::new(status.as_u16(), format!("HTTP {status}: {excerpt}"), status.is_server_error())
}

// --- packing a directory ---------------------------------------------------

/// A staging path in the system temp directory.
///
/// Named from the process id and the clock rather than created and held open:
/// the archive is written by path, and a second push in the same process must not
/// land on the first one's file.
fn temp_archive_path() -> PathBuf {
    let nanos = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0);
    std::env::temp_dir().join(format!("applab-upload-{}-{nanos}.tar", std::process::id()))
}

/// Walk a tree and write it out as a tar. Blocking; see
/// [`package_directory`].
fn pack(root: &Path, archive: &Path) -> Result<(), AppLabError> {
    if !root.is_dir() {
        return Err(AppLabError::local(format!("{} is not a directory", root.display())));
    }

    let mut files: Vec<(String, PathBuf)> = Vec::new();
    collect(root, root, &mut files)?;
    // Sorted by the name that goes into the archive, so the order does not depend
    // on the filesystem's — which is what makes an unchanged tree an empty diff.
    files.sort_by(|a, b| a.0.cmp(&b.0));

    let mut out = fs::File::create(archive)?;
    for (name, path) in files {
        if name.len() > TAR_NAME_MAX {
            return Err(AppLabError::local(format!(
                "path is too long for the archive format ({TAR_NAME_MAX} bytes): {name}"
            )));
        }
        let metadata = fs::metadata(&path)?;
        let mtime = metadata
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map(|d| d.as_secs())
            .unwrap_or(0);
        out.write_all(&tar_header(&name, metadata.len(), file_mode(&metadata), mtime))?;

        let mut file = fs::File::open(&path)?;
        // Buffered by the operating system, and copied rather than read into a
        // Vec: the largest tree this ever packs should not have to fit in memory.
        io::copy(&mut file, &mut out)?;

        // The body is padded to a 512-byte boundary, and the padding is zeroes —
        // a reader walks the archive by the declared sizes, so a header that
        // follows an unpadded body would be read as body.
        let remainder = metadata.len() % 512;
        if remainder != 0 {
            out.write_all(&ZERO_BLOCK[..(512 - remainder) as usize])?;
        }
    }

    // Two zero blocks end the archive. A reader stops at the first, but the
    // format says two, and some are strict enough to say so.
    out.write_all(&ZERO_BLOCK)?;
    out.write_all(&ZERO_BLOCK)?;
    Ok(())
}

/// A 512-byte block of zeroes: tar's padding and its end-of-archive marker.
const ZERO_BLOCK: [u8; 512] = [0u8; 512];

/// tar_header builds the 512-byte header that precedes one file's contents.
///
/// The format is fixed-width: each field is octal digits in a field of a set
/// width, padded with leading zeroes and ended with a NUL (or, for the checksum,
/// a NUL and a space). It is written out rather than taken from a crate for the
/// reason in the module comment — see the Dependencies section.
///
/// `name` is assumed already checked against the 100-byte name field by the
/// caller; a longer path would need the format's prefix mechanism, which is not
/// implemented here.
fn tar_header(name: &str, size: u64, mode: u32, mtime: u64) -> [u8; 512] {
    let mut header = ZERO_BLOCK;

    // The name field is 100 bytes and is NUL-terminated. The slice is exact
    // because the caller checked the length.
    header[..name.len()].copy_from_slice(name.as_bytes());

    // The numeric fields, each in its width. `put_octal` writes the leading
    // zeroes and the terminator so the widths cannot drift from the offsets.
    put_octal(&mut header, 100, 8, u64::from(mode)); // mode
    put_octal(&mut header, 108, 8, 0); // uid
    put_octal(&mut header, 116, 8, 0); // gid
    put_octal(&mut header, 124, 12, size); // size
    put_octal(&mut header, 136, 12, mtime); // mtime

    // The checksum is computed over the header with its own field treated as
    // spaces, so it is written as spaces first and filled in last.
    header[148..156].copy_from_slice(b"        ");

    header[156] = b'0'; // a regular file
    header[257..263].copy_from_slice(b"ustar\0"); // magic
    header[263..265].copy_from_slice(b"00"); // version

    let checksum: u32 = header.iter().map(|byte| u32::from(*byte)).sum();
    // Six octal digits, then NUL, then a space — the one field that is not a
    // plain NUL-terminated number. Some readers accept a trailing NUL instead of
    // the space; writing the canonical form means no reader has to.
    let digits = format!("{checksum:06o}");
    header[148..154].copy_from_slice(digits.as_bytes());
    header[154] = 0;
    header[155] = b' ';

    header
}

/// put_octal writes a number as the fixed-width octal field tar uses: leading
/// zeroes to fill the width, and the terminator the field's size leaves room for.
fn put_octal(header: &mut [u8; 512], offset: usize, width: usize, value: u64) {
    let digits = format!("{:0>width$o}", value, width = width - 1);
    header[offset..offset + width - 1].copy_from_slice(&digits.as_bytes()[..width - 1]);
    header[offset + width - 1] = 0;
}

/// The permission bits to record, on the platforms that have them.
///
/// The executable bit is preserved where the platform has one, so a script in the
/// tree arrives as a script. On a platform without mode bits the constant is
/// what a checked-out file would have had.
#[cfg(unix)]
fn file_mode(metadata: &fs::Metadata) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    metadata.permissions().mode() & 0o777
}

#[cfg(not(unix))]
fn file_mode(_metadata: &fs::Metadata) -> u32 {
    0o644
}

/// Collect a directory's files, depth-first and pruned of [`SKIP_DIRS`].
///
/// Symlinks are skipped, matching the Go client and the shell script: an archive
/// that follows a link out of the tree uploads something the caller did not mean
/// to send.
fn collect(root: &Path, dir: &Path, out: &mut Vec<(String, PathBuf)>) -> Result<(), AppLabError> {
    let mut entries: Vec<fs::DirEntry> = fs::read_dir(dir)?.collect::<Result<Vec<_>, _>>()?;
    entries.sort_by_key(|entry| entry.file_name());

    for entry in entries {
        let name = entry.file_name().to_string_lossy().into_owned();
        if SKIP_DIRS.contains(&name.as_str()) {
            continue;
        }
        let file_type = entry.file_type()?;
        let path = entry.path();
        if file_type.is_dir() {
            collect(root, &path, out)?;
        } else if file_type.is_file() {
            let name = path.strip_prefix(root).unwrap_or(&path).to_string_lossy().replace('\\', "/");
            out.push((name, path));
        }
    }
    Ok(())
}
