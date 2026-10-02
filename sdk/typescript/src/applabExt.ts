/**
 * The parts of the AppLab client a code generator cannot write.
 *
 * This is the TypeScript twin of sdk/python/applab_ext.py, and the reasoning is
 * the same: openapi-generator produces a method per endpoint, which covers most
 * of the API, but three things are not one request.
 *
 *   - A chunked upload. The server needs the part count before the first part
 *     arrives, so the archive has to be staged (or at least measured) first, and
 *     the part size comes from a separate call to /api/v1/config.
 *   - Following a log. A generated method reads a response to its end, which for
 *     `follow=true` is a stream that may never end.
 *   - Packing a directory — a client concern, not an API operation.
 *
 * This file is hand-written and is listed in .openapi-generator-ignore, so
 * regeneration never touches it. It talks to the same API as the generated
 * client and takes the same two things — an address and a key — rather than
 * depending on the generated client's own object shapes, so a generator upgrade
 * cannot silently break it.
 *
 * It uses only Node built-ins: `fetch` (Node 18+), `zlib` and `fs`. There is no
 * dependency to install, which is the point of the fetch generator and would be
 * undone by reaching for a tar library here — so the tar writer below is written
 * out rather than imported.
 *
 *     import { pushDirectory, streamLogs } from "./applabExt";
 *     await pushDirectory(url, key, "shop", "./shop");
 *     for await (const line of streamLogs(url, key, "shop", { follow: true })) {
 *       process.stdout.write(line);
 *     }
 */

import { createReadStream, createWriteStream } from "node:fs";
import { mkdtemp, open, readdir, readFile, rm, stat, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, relative, sep } from "node:path";
import { gzipSync } from "node:zlib";

/** Directories never included in a push. The same set the CLI and Go client use. */
export const SKIP_DIRS = new Set([
  ".git", "node_modules", "target", "dist", "build", ".venv", "venv",
  "__pycache__", ".next", ".nuxt", "vendor", ".idea", ".vscode", ".DS_Store",
]);

/** Fallback part size, matching the server's default, used only when config omits one. */
export const DEFAULT_CHUNK_SIZE = 8 * 1024 * 1024;

/** A failure the deployment reported, with the status and retryability. */
export class AppLabError extends Error {
  // Written out rather than declared as constructor parameter properties, because
  // Node's built-in TypeScript support strips types and does not implement that
  // syntax — a file using it cannot be imported directly by `node app.ts`, which
  // is how a lot of scripts will reach for this.
  readonly status: number;
  readonly retryable: boolean;

  constructor(status: number, message: string, retryable = false) {
    super(message);
    this.name = "AppLabError";
    this.status = status;
    this.retryable = retryable;
  }
}

/** The deployment's limits, as far as the helpers need them. */
export interface AppLabConfig {
  chunkSize: number;
  maxSimpleUpload: number;
  maxChunkBytes: number;
}

export interface UploadResult {
  commit_sha: string;
  message?: string;
  files?: number;
  bytes?: number;
  stripped_root?: string;
}

export interface StreamLogsOptions {
  follow?: boolean;
  pod?: string;
  container?: string;
  tail?: number;
  previous?: boolean;
  since?: string;
}

function authHeaders(key: string, extra: Record<string, string> = {}): Record<string, string> {
  // The key travels in a header and nowhere else: a query parameter is written to
  // access logs, kept in shell history and sent in Referer headers.
  return { Authorization: `Bearer ${key}`, Accept: "application/json", ...extra };
}

async function errorFrom(resp: Response): Promise<AppLabError> {
  const text = await resp.text();
  try {
    const payload = JSON.parse(text);
    if (payload && typeof payload.error === "string") {
      return new AppLabError(resp.status, payload.error, Boolean(payload.retryable));
    }
  } catch {
    // A body that is not JSON: a proxy's HTML, a 413 from something in front of
    // the deployment. The status and an excerpt are more useful than a parse error.
  }
  return new AppLabError(resp.status, `HTTP ${resp.status} ${resp.statusText}: ${text.slice(0, 200)}`, resp.status >= 500);
}

async function unwrap<T>(resp: Response): Promise<T> {
  if (!resp.ok) throw await errorFrom(resp);
  const text = await resp.text();
  if (!text) return undefined as T;
  const payload = JSON.parse(text);
  return (payload && typeof payload === "object" && "data" in payload ? payload.data : payload) as T;
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const search = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null) search.set(k, String(v));
  }
  const s = search.toString();
  return s ? `?${s}` : "";
}

async function getConfig(baseUrl: string, key: string): Promise<AppLabConfig> {
  const base = baseUrl.replace(/\/$/, "");
  const data = await unwrap<Record<string, number>>(
    await fetch(`${base}/api/v1/config`, { headers: authHeaders(key) }),
  );
  return {
    chunkSize: Number(data?.chunk_size ?? DEFAULT_CHUNK_SIZE) || DEFAULT_CHUNK_SIZE,
    maxSimpleUpload: Number(data?.max_simple_upload ?? 0),
    maxChunkBytes: Number(data?.max_chunk_bytes ?? 0),
  };
}

// --- a minimal tar writer --------------------------------------------------
//
// There is no tar in Node's standard library, and pulling in a package would put
// a dependency on the one SDK that is otherwise dependency-free. The tar format
// is simple enough to write directly: a 512-byte header per file, the contents
// padded to 512 bytes, and two zero blocks to end. Only what a source archive
// needs is implemented — regular files, no links, no PAX extension headers,
// since a file name longer than the 100-byte field is the only case that would
// need them and this refuses rather than mis-writing it.

function tarHeader(name: string, size: number, mode: number, mtimeSeconds: number): Buffer {
  const header = Buffer.alloc(512);
  const write = (offset: number, length: number, value: string) => {
    header.write(value, offset, Math.min(length, Buffer.byteLength(value)), "utf8");
  };
  // The name field is 100 bytes; the "prefix" mechanism for longer paths is not
  // implemented, so a path that does not fit is rejected by the caller.
  write(0, 100, name);
  write(100, 8, mode.toString(8).padStart(7, "0") + "\0");
  write(108, 8, "0000000\0"); // uid
  write(116, 8, "0000000\0"); // gid
  write(124, 12, size.toString(8).padStart(11, "0") + "\0");
  write(136, 12, mtimeSeconds.toString(8).padStart(11, "0") + "\0");
  header.write("        ", 148, 8, "utf8"); // checksum is spaces while computed
  write(156, 1, "0"); // regular file
  write(257, 6, "ustar\0");
  write(263, 2, "00");

  let sum = 0;
  for (const byte of header) sum += byte;
  write(148, 8, sum.toString(8).padStart(6, "0") + "\0 ");
  return header;
}

async function walk(root: string, dir = root): Promise<string[]> {
  const out: string[] = [];
  const entries = await readdir(dir, { withFileTypes: true });
  for (const entry of entries.sort((a, b) => (a.name < b.name ? -1 : 1))) {
    if (SKIP_DIRS.has(entry.name)) continue;
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...(await walk(root, full)));
    } else if (entry.isFile()) {
      out.push(full);
    }
    // Symlinks are skipped, matching the Go client and the shell script: an
    // archive that follows a link out of the tree uploads something the caller
    // did not mean to send.
  }
  return out;
}

/**
 * Tar and gzip a directory into a temporary file, returning its path.
 *
 * Deterministic: entries are walked in sorted order and mtimes are preserved, so
 * the same tree produces the same archive and a re-push of an unchanged directory
 * is a no-op commit rather than a diff of timestamps.
 */
export async function packageDirectory(directory: string): Promise<string> {
  const root = directory.replace(/\/$/, "");
  const files = await walk(root);
  const chunks: Buffer[] = [];

  for (const file of files) {
    const name = relative(root, file).split(sep).join("/");
    if (Buffer.byteLength(name) > 100) {
      throw new AppLabError(0, `path is too long for the archive format (100 bytes): ${name}`);
    }
    const info = await stat(file);
    const body = await readFile(file);
    chunks.push(
      tarHeader(name, body.length, info.mode & 0o777, Math.floor(info.mtimeMs / 1000)),
      body,
      Buffer.alloc((512 - (body.length % 512)) % 512),
    );
  }
  chunks.push(Buffer.alloc(1024)); // two zero blocks end the archive

  const dir = await mkdtemp(join(tmpdir(), "applab-upload-"));
  const path = join(dir, "source.tar.gz");
  await writeFile(path, gzipSync(Buffer.concat(chunks)));
  return path;
}

/** Upload an archive in one request, falling back to chunked on a 413. */
export async function uploadSource(
  baseUrl: string,
  key: string,
  appId: string,
  archivePath: string,
  opts: { message?: string; publish?: boolean; branch?: string } = {},
): Promise<UploadResult> {
  const base = baseUrl.replace(/\/$/, "");
  const params = query({
    message: opts.message,
    branch: opts.branch,
    publish: opts.publish === false ? "false" : undefined,
  });
  const body = await readFile(archivePath);
  const resp = await fetch(`${base}/api/v1/apps/${appId}/source${params}`, {
    method: "POST",
    headers: authHeaders(key, { "Content-Type": "application/gzip" }),
    body,
  });
  if (resp.status === 413) {
    return uploadSourceChunked(baseUrl, key, appId, archivePath, opts);
  }
  return unwrap<UploadResult>(resp);
}

/**
 * Upload an archive in parts and return the resulting commit.
 *
 * The archive is a file on disk because the part count has to be known before the
 * first part is sent, which a stream cannot tell you.
 */
export async function uploadSourceChunked(
  baseUrl: string,
  key: string,
  appId: string,
  archivePath: string,
  opts: { message?: string; publish?: boolean; branch?: string; chunkSize?: number } = {},
): Promise<UploadResult> {
  const base = baseUrl.replace(/\/$/, "");
  const size = (await stat(archivePath)).size;
  const cfg = await getConfig(base, key);
  const partSize = opts.chunkSize && opts.chunkSize > 0 ? opts.chunkSize : cfg.chunkSize;
  if (cfg.maxChunkBytes && partSize > cfg.maxChunkBytes) {
    throw new AppLabError(0, `chunk size ${partSize} exceeds the deployment's limit of ${cfg.maxChunkBytes}`);
  }
  const total = Math.max(1, Math.ceil(size / partSize));

  const started = await unwrap<{ upload_id: string }>(
    await fetch(`${base}/api/v1/apps/${appId}/source/uploads`, {
      method: "POST",
      headers: authHeaders(key, { "Content-Type": "application/json" }),
      body: JSON.stringify({ total, chunk_size: partSize, message: opts.message ?? "" }),
    }),
  );
  if (!started?.upload_id) throw new AppLabError(0, "the deployment did not return an upload id");

  const handle = await open(archivePath, "r");
  try {
    for (let index = 1; index <= total; index++) {
      const part = Buffer.alloc(Math.min(partSize, size - (index - 1) * partSize));
      await handle.read(part, 0, part.length, (index - 1) * partSize);
      // The index is 1-based and is which part this is, not a byte offset; the
      // server rejects a gap.
      await unwrap(
        await fetch(`${base}/api/v1/apps/${appId}/source/uploads/${started.upload_id}/parts/${index}`, {
          method: "PUT",
          headers: authHeaders(key, { "Content-Type": "application/octet-stream" }),
          body: part,
        }),
      );
    }
  } finally {
    await handle.close();
  }

  const params = query({
    message: opts.message,
    branch: opts.branch,
    publish: opts.publish === false ? "false" : undefined,
  });
  return unwrap<UploadResult>(
    await fetch(`${base}/api/v1/apps/${appId}/source/uploads/${started.upload_id}/complete${params}`, {
      method: "POST",
      headers: authHeaders(key, { "Content-Type": "application/json" }),
    }),
  );
}

/** Package a directory and upload it as a commit. The `applab push` path. */
export async function pushDirectory(
  baseUrl: string,
  key: string,
  appId: string,
  directory: string,
  opts: { message?: string; publish?: boolean; branch?: string } = {},
): Promise<UploadResult> {
  const archive = await packageDirectory(directory);
  try {
    return await uploadSourceChunked(baseUrl, key, appId, archive, opts);
  } finally {
    await unlink(archive).catch(() => {});
  }
}

/**
 * Yield an app's log lines, one at a time.
 *
 * An async generator rather than a function returning a string: with
 * `follow: true` the response is a stream that does not end until the app stops
 * logging, and reading it into a string would never return. Iterating lets a
 * caller print each line as it arrives, which is the point of following.
 */
export async function* streamLogs(
  baseUrl: string,
  key: string,
  appId: string,
  opts: StreamLogsOptions = {},
): AsyncGenerator<string> {
  const base = baseUrl.replace(/\/$/, "");
  const params = query({
    follow: opts.follow ? "true" : undefined,
    pod: opts.pod,
    container: opts.container,
    tail: opts.tail,
    previous: opts.previous ? "true" : undefined,
    since: opts.since,
  });
  const resp = await fetch(`${base}/api/v1/apps/${appId}/logs${params}`, {
    headers: authHeaders(key),
  });
  if (!resp.ok) throw await errorFrom(resp);
  if (!resp.body) throw new AppLabError(0, "the deployment returned no body to stream");
  yield* readLines(resp.body);
}

/** A build's log lines. The build variant of streamLogs. */
export async function* streamBuildLogs(
  baseUrl: string,
  key: string,
  appId: string,
  buildId: string,
  opts: { follow?: boolean } = {},
): AsyncGenerator<string> {
  const base = baseUrl.replace(/\/$/, "");
  const params = query({ follow: opts.follow ? "true" : undefined });
  const resp = await fetch(`${base}/api/v1/apps/${appId}/builds/${buildId}/logs${params}`, {
    headers: authHeaders(key),
  });
  if (!resp.ok) throw await errorFrom(resp);
  if (!resp.body) throw new AppLabError(0, "the deployment returned no body to stream");
  yield* readLines(resp.body);
}

/** Decode a fetch body stream into lines, keeping the terminators. */
async function* readLines(body: ReadableStream<Uint8Array>): AsyncGenerator<string> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let newline: number;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        yield buffer.slice(0, newline + 1);
        buffer = buffer.slice(newline + 1);
      }
    }
    if (buffer) yield buffer;
  } finally {
    reader.releaseLock();
  }
}

/**
 * Find the build whose id starts with `prefix`.
 *
 * Builds are shown abbreviated and typed abbreviated. This is strict: two builds
 * sharing a prefix throws rather than picking one, because guessing is how the
 * wrong build gets cancelled.
 */
export async function resolveBuildId(
  baseUrl: string,
  key: string,
  appId: string,
  prefix: string,
  opts: { limit?: number } = {},
): Promise<string> {
  const base = baseUrl.replace(/\/$/, "");
  const params = query({ limit: opts.limit ?? 50 });
  const builds = await unwrap<Array<{ id: string }>>(
    await fetch(`${base}/api/v1/apps/${appId}/builds${params}`, { headers: authHeaders(key) }),
  );
  const matches = (builds ?? []).filter((b) => b.id.startsWith(prefix));
  if (matches.length === 0) throw new AppLabError(404, `no build of ${appId} starts with ${JSON.stringify(prefix)}`);
  if (matches.length > 1) throw new AppLabError(409, `${JSON.stringify(prefix)} matches ${matches.length} builds; be more specific`);
  return matches[0].id;
}
