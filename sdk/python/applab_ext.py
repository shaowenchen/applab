"""The parts of the AppLab client a code generator cannot write.

openapi-generator produces a method per endpoint from api/openapi.yaml, and that
covers most of the API. Three things it cannot cover, because they are not one
request:

  * A chunked upload. The server needs the part count *before* the first part
    arrives, so the archive has to be staged to disk (or at least measured)
    first, and the part size comes from a separate call to /api/v1/config. A
    generated one-request method for POST /source cannot do that.
  * Following a log. The generated client reads a response to its end, which for
    `follow=true` is a stream that may never end — it would buffer forever or hang.
  * Packing a directory. That is a client concern, not an API operation.

So this module is hand-written and is *not* touched by regeneration (it is listed
in .openapi-generator-ignore). It talks to the same API as the generated client
and takes the same two things — an address and a key — rather than depending on
the generated client's own object shapes, so a generator upgrade cannot silently
break it.

    from applab_ext import push_directory, stream_logs

    url, key = "https://applab.example.com/applab", os.environ["APPLAB_KEY"]
    push_directory(url, key, "shop", "./shop")
    for line in stream_logs(url, key, "shop", follow=True):
        print(line, end="")

Only the standard library is used. That is deliberate: this file has to run
wherever someone installs the SDK, and a helper that needed a dependency the SDK
did not already have would be the one thing that does not import.
"""

from __future__ import annotations

import json
import os
import socket
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Iterator, Optional

# The skip set the CLI, the Go client and the console all use. It is repeated
# here rather than fetched because it is a client-side decision about what a
# person means by "this directory", not something the server can know.
SKIP_DIRS = {
    ".git", "node_modules", "target", "dist", "build", ".venv", "venv",
    "__pycache__", ".next", ".nuxt", "vendor", ".idea", ".vscode", ".DS_Store",
}

# The fallback part size, used only when the deployment does not report one. It
# matches the server's own default (8 MiB).
DEFAULT_CHUNK_SIZE = 8 * 1024 * 1024


class AppLabError(RuntimeError):
    """A failure the deployment reported.

    Carries the status and whether the server considers the call retryable, so a
    caller can branch on them rather than parsing the message.
    """

    def __init__(self, status: int, message: str, retryable: bool = False):
        super().__init__(message)
        self.status = status
        self.message = message
        self.retryable = retryable


@dataclass
class Config:
    """The deployment's self-description, as far as the helpers need it."""

    chunk_size: int
    max_simple_upload: int
    max_chunk_bytes: int


def _request(
    base_url: str,
    key: str,
    method: str,
    path: str,
    *,
    body: Optional[bytes] = None,
    content_type: Optional[str] = None,
    query: Optional[dict] = None,
    stream: bool = False,
):
    """Perform one request and decode the envelope.

    The key travels in a header and nowhere else — never a query parameter, which
    is written to access logs, kept in shell history and sent in Referer headers.
    """
    url = base_url.rstrip("/") + path
    if query:
        url += "?" + urllib.parse.urlencode({k: v for k, v in query.items() if v is not None})

    req = urllib.request.Request(url, data=body, method=method)
    req.add_header("Authorization", "Bearer " + key)
    req.add_header("Accept", "application/json")
    if content_type:
        req.add_header("Content-Type", content_type)

    try:
        resp = urllib.request.urlopen(req)
    except urllib.error.HTTPError as err:
        raise _error_from(err) from None
    except urllib.error.URLError as err:
        # A transport failure — the deployment could not be reached at all. It is
        # the only case where there is no status to report, and it is retryable in
        # the sense that the same call may well work once the network does.
        raise AppLabError(0, f"cannot reach {base_url}: {err.reason}", retryable=True) from None

    if stream:
        return resp
    with resp:
        return _decode(resp.read())


def _error_from(err: urllib.error.HTTPError) -> AppLabError:
    """Turn an HTTP failure into an AppLabError, reading the error envelope.

    A body that is not JSON — a proxy's HTML, a 413 from something in front of the
    deployment — is reported by status and an excerpt, which is more useful than
    an exception from the JSON decoder.
    """
    try:
        payload = json.loads(err.read() or b"{}")
        if isinstance(payload, dict) and "error" in payload:
            return AppLabError(err.code, str(payload["error"]), bool(payload.get("retryable")))
    except (ValueError, OSError):
        pass
    return AppLabError(err.code, f"HTTP {err.code} {err.reason}", retryable=err.code >= 500)


def _decode(raw: bytes):
    """Unwrap the {"data": ...} envelope every ordinary route answers with."""
    if not raw:
        return None
    payload = json.loads(raw)
    if isinstance(payload, dict) and "data" in payload:
        return payload["data"]
    return payload


def get_config(base_url: str, key: str) -> Config:
    """Read the deployment's limits, which the chunked upload needs."""
    data = _request(base_url, key, "GET", "/api/v1/config") or {}
    return Config(
        chunk_size=int(data.get("chunk_size") or DEFAULT_CHUNK_SIZE),
        max_simple_upload=int(data.get("max_simple_upload") or 0),
        max_chunk_bytes=int(data.get("max_chunk_bytes") or 0),
    )


def package_directory(directory: str, *, gzip: bool = True) -> str:
    """Tar a directory into a temporary file and return its path.

    The result is deterministic — entries are sorted, names are relative and
    forward-slashed, and mtimes are kept — so the same tree produces the same
    archive and a re-push of an unchanged directory is a no-op commit rather than
    a diff of timestamps.

    The caller owns the file and should delete it.
    """
    directory = os.path.abspath(directory)
    if not os.path.isdir(directory):
        raise AppLabError(0, f"{directory} is not a directory")

    suffix = ".tar.gz" if gzip else ".tar"
    fd, path = tempfile.mkstemp(prefix="applab-upload-", suffix=suffix)
    mode = "w:gz" if gzip else "w"
    try:
        with os.fdopen(fd, "wb") as raw, tarfile.open(fileobj=raw, mode=mode) as tar:
            for root, dirs, files in os.walk(directory):
                # Pruned in place so os.walk does not descend into them.
                dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
                for name in sorted(files):
                    full = os.path.join(root, name)
                    rel = os.path.relpath(full, directory).replace(os.sep, "/")
                    if os.path.islink(full):
                        continue
                    tar.add(full, arcname=rel, recursive=False)
    except BaseException:
        os.unlink(path)
        raise
    return path


def push_directory(
    base_url: str,
    key: str,
    app_id: str,
    directory: str,
    *,
    message: Optional[str] = None,
    publish: bool = True,
    branch: Optional[str] = None,
) -> dict:
    """Package a directory and upload it as a commit. Returns the upload result.

    This is the `applab push` path: what the Go client and the seeded applab.sh do
    from a terminal. It always uses the chunked upload, even for a small tree,
    because the chunked path has no size limit and choosing between two paths on
    a size the server has not been asked about yet is how a push fails at exactly
    the wrong moment.
    """
    archive = package_directory(directory, gzip=True)
    try:
        return upload_source_chunked(
            base_url, key, app_id, archive, message=message, publish=publish, branch=branch
        )
    finally:
        os.unlink(archive)


def upload_source_chunked(
    base_url: str,
    key: str,
    app_id: str,
    archive_path: str,
    *,
    message: Optional[str] = None,
    publish: bool = True,
    branch: Optional[str] = None,
    chunk_size: Optional[int] = None,
) -> dict:
    """Upload a source archive in parts and return the resulting commit.

    The archive must already exist as a file: the part count has to be known
    before the first part is sent, which cannot be done from a stream without
    staging it somewhere first.
    """
    size = os.path.getsize(archive_path)
    cfg = get_config(base_url, key)
    part_size = chunk_size or cfg.chunk_size
    if part_size <= 0:
        part_size = DEFAULT_CHUNK_SIZE
    if cfg.max_chunk_bytes and part_size > cfg.max_chunk_bytes:
        raise AppLabError(0, f"chunk size {part_size} exceeds the deployment's limit of {cfg.max_chunk_bytes}")

    total = max(1, (size + part_size - 1) // part_size)

    started = _request(
        base_url,
        key,
        "POST",
        f"/api/v1/apps/{app_id}/source/uploads",
        body=json.dumps({"total": total, "chunk_size": part_size, "message": message or ""}).encode(),
        content_type="application/json",
    )
    upload_id = (started or {}).get("upload_id")
    if not upload_id:
        raise AppLabError(0, "the deployment did not return an upload id")

    base = f"/api/v1/apps/{app_id}/source/uploads/{upload_id}"
    with open(archive_path, "rb") as archive:
        for index in range(1, total + 1):
            part = archive.read(part_size)
            # The index is 1-based and is not the byte offset — it is which part
            # this is, and the server rejects a gap.
            _request(
                base_url,
                key,
                "PUT",
                f"{base}/parts/{index}",
                body=part,
                content_type="application/octet-stream",
            )

    return _request(
        base_url,
        key,
        "POST",
        f"{base}/complete",
        query={"message": message, "branch": branch, "publish": "" if publish else "false"},
    )


def upload_source(
    base_url: str,
    key: str,
    app_id: str,
    archive_path: str,
    *,
    message: Optional[str] = None,
    publish: bool = True,
    branch: Optional[str] = None,
) -> dict:
    """Upload an archive in one request.

    For a tree under the deployment's own limit this is one call instead of the
    several a chunked upload makes. Over the limit the server answers 413 and names
    the chunked endpoints; that is turned into a redirect to
    upload_source_chunked, so a caller that guessed "small enough" is not left to
    handle it.
    """
    with open(archive_path, "rb") as archive:
        body = archive.read()
    try:
        return _request(
            base_url,
            key,
            "POST",
            f"/api/v1/apps/{app_id}/source",
            body=body,
            content_type="application/gzip",
            query={"message": message, "branch": branch, "publish": "" if publish else "false"},
        )
    except AppLabError as err:
        if err.status == 413:
            return upload_source_chunked(
                base_url, key, app_id, archive_path, message=message, publish=publish, branch=branch
            )
        raise


def stream_logs(
    base_url: str,
    key: str,
    app_id: str,
    *,
    follow: bool = False,
    pod: Optional[str] = None,
    container: Optional[str] = None,
    tail: Optional[int] = None,
    previous: bool = False,
    since: Optional[str] = None,
) -> Iterator[str]:
    """Yield an app's log lines, one at a time.

    A generator rather than a function returning a string, because with
    `follow=True` the response is a stream that does not end until the app stops
    logging — reading it into a string would never return. Iterating lets a caller
    print each line as it arrives, which is the whole point of following.
    """
    resp = _request(
        base_url,
        key,
        "GET",
        f"/api/v1/apps/{app_id}/logs",
        query={
            "follow": "true" if follow else None,
            "pod": pod,
            "container": container,
            "tail": tail,
            "previous": "true" if previous else None,
            "since": since,
        },
        stream=True,
    )
    try:
        for line in resp:
            yield line.decode("utf-8", "replace")
    finally:
        resp.close()


def stream_build_logs(
    base_url: str, key: str, app_id: str, build_id: str, *, follow: bool = False
) -> Iterator[str]:
    """Yield a build's log lines. The build variant of stream_logs."""
    resp = _request(
        base_url,
        key,
        "GET",
        f"/api/v1/apps/{app_id}/builds/{build_id}/logs",
        query={"follow": "true" if follow else None},
        stream=True,
    )
    try:
        for line in resp:
            yield line.decode("utf-8", "replace")
    finally:
        resp.close()


def resolve_build_id(base_url: str, key: str, app_id: str, prefix: str, *, limit: int = 50) -> str:
    """Find the build whose id starts with `prefix`.

    Builds are shown abbreviated and typed abbreviated. This is what turns what a
    person copied off a screen into the id the API wants, and it is deliberately
    strict: two builds sharing a prefix raises rather than picking one, because
    guessing is how the wrong build gets cancelled.
    """
    builds = _request(
        base_url, key, "GET", f"/api/v1/apps/{app_id}/builds", query={"limit": limit}
    ) or []
    matches = [b.get("id", "") for b in builds if str(b.get("id", "")).startswith(prefix)]
    if not matches:
        raise AppLabError(404, f"no build of {app_id} starts with {prefix!r}")
    if len(matches) > 1:
        raise AppLabError(409, f"{prefix!r} matches {len(matches)} builds; be more specific")
    return matches[0]
