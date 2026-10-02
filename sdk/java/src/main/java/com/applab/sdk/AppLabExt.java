package com.applab.sdk;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.io.UncheckedIOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.FileVisitResult;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.SimpleFileVisitor;
import java.nio.file.attribute.BasicFileAttributes;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.NoSuchElementException;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.stream.Stream;
import java.util.stream.StreamSupport;
import java.util.zip.GZIPOutputStream;

/**
 * The parts of the AppLab client a code generator cannot write.
 *
 * <p>This is the Java twin of {@code sdk/python/applab_ext.py} and
 * {@code sdk/typescript/src/applabExt.ts}, and the reasoning is the same.
 * openapi-generator produces a method per endpoint, which covers most of the API,
 * but four things are not one request:
 *
 * <ul>
 *   <li>A chunked upload. The server needs the part count <em>before</em> the
 *       first part arrives, so the archive has to be staged to disk first, and the
 *       part size comes from a separate call to {@code /api/v1/config}. A
 *       generated one-request method for {@code POST /source} cannot do that.</li>
 *   <li>Following a log. A generated method reads a response to its end, which for
 *       {@code follow=true} is a stream that may never end — it would hang.</li>
 *   <li>Packing a directory. That is a client concern, not an API operation.</li>
 *   <li>Resolving an abbreviated build id, which is a lookup and a conflict check
 *       rather than a single call.</li>
 * </ul>
 *
 * <p>This file is hand-written and is listed in {@code .openapi-generator-ignore},
 * so regeneration never touches it. It talks to the same API as the generated
 * client and takes the same two things — an address and a key — rather than
 * depending on the generated client's own classes, so a generator upgrade cannot
 * silently break it.
 *
 * <p>Only the JDK is used ({@link java.net.http.HttpClient}, {@link java.util.zip},
 * {@link java.nio.file}). That is deliberate and matches the Python and TypeScript
 * helpers: this file has to compile wherever someone adds the SDK, and a helper
 * that needed a dependency the SDK did not already have would be the one thing
 * that does not.
 *
 * <p>For the same reason there is no JSON library here. The generated client
 * parses responses properly; what this file needs from a response is one or two
 * named fields out of an envelope this repository defines, and it picks those out
 * of the text. That is narrower than a parser and is pointed only at the API's own
 * responses — see the note above the small readers near the end of the file.
 *
 * <pre>{@code
 * AppLabExt.uploadSourceChunked(url, key, "shop", Paths.get("source.tar.gz"), opts);
 * try (Stream<String> lines = AppLabExt.streamLogs(url, key, "shop", LogOptions.following())) {
 *     lines.forEach(System.out::print);
 * }
 * }</pre>
 */
public final class AppLabExt {

    /**
     * The fallback part size, used only when the deployment does not report one.
     * It matches the server's own default (8 MiB).
     */
    public static final int DEFAULT_CHUNK_SIZE = 8 * 1024 * 1024;

    /**
     * Directories never included in a push. The same set the CLI, the Go client
     * and the console use. It is repeated here rather than fetched because it is a
     * client-side decision about what a person means by "this directory", not
     * something the server can know.
     */
    public static final List<String> SKIP_DIRS = List.of(
            ".git", "node_modules", "target", "dist", "build", ".venv", "venv",
            "__pycache__", ".next", ".nuxt", "vendor", ".idea", ".vscode", ".DS_Store");

    /** The name field of a tar header is 100 bytes; a longer path is refused. */
    private static final int TAR_NAME_MAX = 100;

    /**
     * One client for the class, not one per request.
     *
     * <p>{@link HttpClient} owns a connection pool and some threads, so building
     * one per call would open a fresh connection for every part of a chunked
     * upload — the case with the most requests and the largest bodies. It is
     * thread-safe, which is what makes a single instance right rather than a
     * convenience.
     *
     * <p>A caller that needs its own timeouts or proxy settings should use the
     * generated client, which is constructed with them; these helpers are
     * deliberately the version that works out of the box.
     */
    private static final HttpClient CLIENT = HttpClient.newHttpClient();

    private static final Pattern DATA_FIELD = Pattern.compile("\"data\"\\s*:");
    private static final Pattern ERROR_FIELD = Pattern.compile("\"error\"\\s*:");
    private static final Pattern RETRYABLE_FIELD = Pattern.compile("\"retryable\"\\s*:\\s*true");
    private static final Pattern CHUNK_SIZE_FIELD = Pattern.compile("\"chunk_size\"\\s*:\\s*(\\d+)");
    private static final Pattern MAX_SIMPLE_FIELD = Pattern.compile("\"max_simple_upload\"\\s*:\\s*(\\d+)");
    private static final Pattern MAX_CHUNK_FIELD = Pattern.compile("\"max_chunk_bytes\"\\s*:\\s*(\\d+)");
    private static final Pattern UPLOAD_ID_FIELD = Pattern.compile("\"upload_id\"\\s*:\\s*\"([^\"]*)\"");
    private static final Pattern BUILD_ID_FIELD = Pattern.compile("\"id\"\\s*:\\s*\"([^\"]*)\"");

    private AppLabExt() {
        // A holder for static helpers; there is no instance state to construct.
    }

    /** A failure the deployment reported, carrying the status and retryability. */
    public static final class AppLabException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        private final int status;
        private final boolean retryable;

        public AppLabException(int status, String message, boolean retryable) {
            super(message);
            this.status = status;
            this.retryable = retryable;
        }

        /** The HTTP status, or 0 when the deployment could not be reached at all. */
        public int status() {
            return status;
        }

        /** Whether the server considers the same call worth retrying. */
        public boolean retryable() {
            return retryable;
        }
    }

    /** The deployment's self-description, as far as the helpers need it. */
    public static final class Config {
        public final int chunkSize;
        public final int maxSimpleUpload;
        public final int maxChunkBytes;

        Config(int chunkSize, int maxSimpleUpload, int maxChunkBytes) {
            this.chunkSize = chunkSize;
            this.maxSimpleUpload = maxSimpleUpload;
            this.maxChunkBytes = maxChunkBytes;
        }
    }

    /** What a source upload produced. */
    public static final class UploadResult {
        /** The branch the stored commit is on. */
        public final String branch;
        /** The commit the deployment stored. */
        public final String commitSha;
        /** The commit message the deployment recorded. */
        public final String message;
        /** How many files the archive contained. */
        public final int files;
        /** How many bytes were stored. */
        public final long bytes;
        /** The single top-level directory the server removed, if it removed one. */
        public final String strippedRoot;

        UploadResult(String branch, String commitSha, String message, int files, long bytes, String strippedRoot) {
            this.branch = branch;
            this.commitSha = commitSha;
            this.message = message;
            this.files = files;
            this.bytes = bytes;
            this.strippedRoot = strippedRoot;
        }
    }

    /** What a caller may say about an upload. */
    public static final class UploadOptions {
        /** The commit message. */
        public String message;
        /**
         * Whether the server should build and deploy the commit it stores. The
         * server does so when this is left at its default of {@code true}; see
         * {@link #publish(boolean)} for why a caller would ever turn it off.
         */
        public boolean publish = true;
        /** The branch to store the commit on. Null means the app's default. */
        public String branch;
        /**
         * The part size to use, overriding the deployment's advertised one. Zero
         * or negative means "ask the deployment".
         */
        public int chunkSize;

        public UploadOptions message(String value) {
            this.message = value;
            return this;
        }

        /**
         * Asks the server not to build and deploy what it stores.
         *
         * <p>Set this false only when the caller ships the commit itself — the CLI
         * uploads and then calls the build and deploy endpoints by name, so an
         * upload that also published would be built twice and the caller would be
         * watching a build they did not start. Leaving it alone is what makes
         * changing an app's source over the API mean what it means over git.
         */
        public UploadOptions publish(boolean value) {
            this.publish = value;
            return this;
        }

        public UploadOptions branch(String value) {
            this.branch = value;
            return this;
        }

        public UploadOptions chunkSize(int value) {
            this.chunkSize = value;
            return this;
        }
    }

    /** What a caller may say about a log stream. */
    public static final class LogOptions {
        /**
         * Keep the response open as new lines arrive. This is the whole reason the
         * helper exists: a plain client would buffer a response that may never end.
         */
        public boolean follow;
        public String pod;
        public String container;
        public Integer tail;
        /** Read the previous container's logs, for a pod that has restarted. */
        public boolean previous;
        /** An RFC 3339 timestamp or a duration such as {@code 5m}. */
        public String since;

        /** The default: existing lines, then stop. */
        public static LogOptions once() {
            return new LogOptions();
        }

        /** Keep streaming after the existing lines. */
        public static LogOptions following() {
            return new LogOptions().follow(true);
        }

        public LogOptions follow(boolean value) {
            this.follow = value;
            return this;
        }

        public LogOptions pod(String value) {
            this.pod = value;
            return this;
        }

        public LogOptions container(String value) {
            this.container = value;
            return this;
        }

        public LogOptions tail(int value) {
            this.tail = value;
            return this;
        }

        public LogOptions previous(boolean value) {
            this.previous = value;
            return this;
        }

        public LogOptions since(String value) {
            this.since = value;
            return this;
        }
    }

    // --- the package-private plumbing ---------------------------------------

    /**
     * Read the deployment's limits, which the chunked upload needs.
     *
     * <p>A limit the deployment does not report comes back as 0, meaning "no limit
     * known" — the helpers then trust the deployment to refuse rather than
     * inventing a ceiling of their own.
     */
    public static Config getConfig(String baseUrl, String key) {
        Response resp = send(baseUrl, key, "GET", "/api/v1/config", null, null, null, false);
        String body = resp.bodyText;
        int chunkSize = intField(CHUNK_SIZE_FIELD, body, DEFAULT_CHUNK_SIZE);
        if (chunkSize <= 0) {
            chunkSize = DEFAULT_CHUNK_SIZE;
        }
        return new Config(
                chunkSize,
                intField(MAX_SIMPLE_FIELD, body, 0),
                intField(MAX_CHUNK_FIELD, body, 0));
    }

    /**
     * Tar and gzip a directory into a temporary file, returning its path.
     *
     * <p>The result is deterministic — entries are walked in sorted order, names
     * are relative and forward-slashed, and mtimes are preserved — so the same
     * tree produces the same archive and a re-push of an unchanged directory is a
     * no-op commit rather than a diff of timestamps.
     *
     * <p>The tar writer is written out below rather than imported, because the JDK
     * has no tar and adding a dependency would undo the reason this SDK chose the
     * generator's {@code native} library. The format is simple enough: a 512-byte
     * header per file, the contents padded to a 512-byte boundary, and two zero
     * blocks to end.
     *
     * <p>The caller owns the file and should delete it.
     */
    public static Path packageDirectory(String directory) {
        Path root = Paths.get(directory).toAbsolutePath().normalize();
        if (!Files.isDirectory(root)) {
            throw new AppLabException(0, root + " is not a directory", false);
        }

        // Written straight through to the staging file rather than assembled in
        // memory: the chunked upload exists for trees too large for one request,
        // and packing one of those into a byte array would be the moment the
        // client needed the most memory — the same trap the Go client's spooling
        // note describes. Each file is streamed in, so memory stays at one buffer.
        Path path = null;
        try {
            path = Files.createTempFile("applab-upload-", ".tar.gz");
            try (OutputStream raw = Files.newOutputStream(path);
                 GZIPOutputStream gz = new GZIPOutputStream(raw)) {
                writeTar(root, gz);
            }
            return path;
        } catch (IOException err) {
            if (path != null) {
                try {
                    Files.deleteIfExists(path);
                } catch (IOException ignored) {
                    // Reporting the original failure matters more than this one.
                }
            }
            throw new AppLabException(0, "could not pack " + root + ": " + err.getMessage(), false);
        }
    }

    /**
     * Package a directory and upload it as a commit. This is the {@code applab push}
     * path.
     *
     * <p>It always uses the chunked upload, even for a small tree, because the
     * chunked path has no size limit and choosing between two paths on a size the
     * server has not been asked about yet is how a push fails at exactly the wrong
     * moment.
     */
    public static UploadResult pushDirectory(String baseUrl, String key, String appId, String directory, UploadOptions opts) {
        UploadOptions options = opts == null ? new UploadOptions() : opts;
        Path archive = packageDirectory(directory);
        try {
            return uploadSourceChunked(baseUrl, key, appId, archive, options);
        } finally {
            try {
                Files.deleteIfExists(archive);
            } catch (IOException ignored) {
                // The upload already succeeded; failing to clean up a temp file is
                // not a reason to report the push as failed.
            }
        }
    }

    /**
     * Upload a source archive in one request.
     *
     * <p>For a tree under the deployment's own limit this is one call instead of
     * the several a chunked upload makes. Over the limit the server answers 413
     * and names the chunked endpoints; that is turned into a redirect to
     * {@link #uploadSourceChunked}, so a caller that guessed "small enough" is not
     * left to handle it.
     */
    public static UploadResult uploadSource(String baseUrl, String key, String appId, Path archivePath, UploadOptions opts) {
        UploadOptions options = opts == null ? new UploadOptions() : opts;
        byte[] body;
        try {
            body = Files.readAllBytes(archivePath);
        } catch (IOException err) {
            throw new AppLabException(0, "could not read " + archivePath + ": " + err.getMessage(), false);
        }

        Map<String, String> query = uploadQuery(options);
        try {
            Response resp = send(baseUrl, key, "POST", "/api/v1/apps/" + appId + "/source",
                    query, body, "application/gzip", false);
            return uploadResult(resp.bodyText);
        } catch (AppLabException err) {
            if (err.status() == 413) {
                return uploadSourceChunked(baseUrl, key, appId, archivePath, options);
            }
            throw err;
        }
    }

    /**
     * Upload a source archive in parts and return the resulting commit.
     *
     * <p>The archive must already exist as a file: the part count has to be known
     * before the first part is sent, which cannot be done from a stream without
     * staging it somewhere first. This is a requirement of the protocol rather
     * than a convenience — and it is why {@link #pushDirectory} packs to a
     * temporary file before it calls this.
     */
    public static UploadResult uploadSourceChunked(String baseUrl, String key, String appId, Path archivePath, UploadOptions opts) {
        UploadOptions options = opts == null ? new UploadOptions() : opts;

        long size;
        try {
            size = Files.size(archivePath);
        } catch (IOException err) {
            throw new AppLabException(0, "could not measure " + archivePath + ": " + err.getMessage(), false);
        }

        Config cfg = getConfig(baseUrl, key);
        int partSize = options.chunkSize > 0 ? options.chunkSize : cfg.chunkSize;
        if (partSize <= 0) {
            partSize = DEFAULT_CHUNK_SIZE;
        }
        if (cfg.maxChunkBytes > 0 && partSize > cfg.maxChunkBytes) {
            throw new AppLabException(0,
                    "chunk size " + partSize + " exceeds the deployment's limit of " + cfg.maxChunkBytes, false);
        }
        int total = (int) Math.max(1L, (size + partSize - 1) / partSize);

        String begin = "{\"total\":" + total + ",\"chunk_size\":" + partSize + ",\"message\":\""
                + jsonEscape(options.message == null ? "" : options.message) + "\"}";
        Response started = send(baseUrl, key, "POST", "/api/v1/apps/" + appId + "/source/uploads",
                null, begin.getBytes(StandardCharsets.UTF_8), "application/json", false);
        String uploadId = stringField(UPLOAD_ID_FIELD, started.bodyText, 1);
        if (uploadId == null || uploadId.isEmpty()) {
            throw new AppLabException(0, "the deployment did not return an upload id", false);
        }

        String basePath = "/api/v1/apps/" + appId + "/source/uploads/" + uploadId;
        byte[] part = new byte[partSize];
        try (InputStream in = Files.newInputStream(archivePath)) {
            for (int index = 1; index <= total; index++) {
                int filled = 0;
                while (filled < partSize) {
                    int read = in.read(part, filled, partSize - filled);
                    if (read < 0) {
                        break;
                    }
                    filled += read;
                }
                byte[] slice = filled == partSize ? part : java.util.Arrays.copyOf(part, filled);
                // The index is 1-based and is which part this is, not a byte
                // offset; the server rejects a gap.
                send(baseUrl, key, "PUT", basePath + "/parts/" + index,
                        null, slice, "application/octet-stream", false);
            }
        } catch (IOException err) {
            throw new AppLabException(0, "could not read " + archivePath + ": " + err.getMessage(), false);
        }

        Response done = send(baseUrl, key, "POST", basePath + "/complete",
                uploadQuery(options), null, null, false);
        return uploadResult(done.bodyText);
    }

    /**
     * An app's log lines, one at a time.
     *
     * <p>A stream of lines rather than a string, because with {@code follow=true}
     * the response does not end until the app stops logging — reading it into a
     * string would never return. Iterating lets a caller print each line as it
     * arrives, which is the whole point of following.
     *
     * <p>The response body is read incrementally ({@code ofInputStream}, wrapped
     * in a reader) rather than through {@code ofString}, which would wait for the
     * end of a stream that may not have one.
     *
     * <p>Close the stream to release the connection; a try-with-resources block
     * around a consuming terminal operation is the intended use.
     */
    public static Stream<String> streamLogs(String baseUrl, String key, String appId, LogOptions opts) {
        LogOptions options = opts == null ? new LogOptions() : opts;
        Map<String, String> query = new HashMap<>();
        query.put("follow", options.follow ? "true" : null);
        query.put("pod", options.pod);
        query.put("container", options.container);
        query.put("tail", options.tail == null ? null : options.tail.toString());
        query.put("previous", options.previous ? "true" : null);
        query.put("since", options.since);

        Response resp = send(baseUrl, key, "GET", "/api/v1/apps/" + appId + "/logs",
                query, null, null, true);
        return readLines(resp);
    }

    /** A build's log lines. The build variant of {@link #streamLogs}. */
    public static Stream<String> streamBuildLogs(String baseUrl, String key, String appId, String buildId, boolean follow) {
        Map<String, String> query = new HashMap<>();
        query.put("follow", follow ? "true" : null);
        Response resp = send(baseUrl, key, "GET", "/api/v1/apps/" + appId + "/builds/" + buildId + "/logs",
                query, null, null, true);
        return readLines(resp);
    }

    /**
     * Find the build whose id starts with {@code prefix}.
     *
     * <p>Builds are shown abbreviated and typed abbreviated. This is what turns
     * what a person copied off a screen into the id the API wants, and it is
     * deliberately strict: two builds sharing a prefix raises rather than picking
     * one, because guessing is how the wrong build gets cancelled.
     */
    public static String resolveBuildId(String baseUrl, String key, String appId, String prefix) {
        return resolveBuildId(baseUrl, key, appId, prefix, 50);
    }

    /** {@link #resolveBuildId(String, String, String, String)} with a page size. */
    public static String resolveBuildId(String baseUrl, String key, String appId, String prefix, int limit) {
        Map<String, String> query = new HashMap<>();
        query.put("limit", Integer.toString(limit));
        Response resp = send(baseUrl, key, "GET", "/api/v1/apps/" + appId + "/builds",
                query, null, null, false);

        List<String> matches = new ArrayList<>();
        Matcher matcher = BUILD_ID_FIELD.matcher(resp.bodyText);
        while (matcher.find()) {
            String id = matcher.group(1);
            if (id.startsWith(prefix)) {
                matches.add(id);
            }
        }
        if (matches.isEmpty()) {
            throw new AppLabException(404, "no build of " + appId + " starts with " + prefix, false);
        }
        if (matches.size() > 1) {
            throw new AppLabException(409, prefix + " matches " + matches.size() + " builds; be more specific", false);
        }
        return matches.get(0);
    }

    // --- one request, and the envelope --------------------------------------

    /** One response: its status and its body, already read (or ready to read). */
    private static final class Response {
        final int status;
        final String bodyText;
        final InputStream stream;

        Response(int status, String bodyText, InputStream stream) {
            this.status = status;
            this.bodyText = bodyText;
            this.stream = stream;
        }
    }

    /**
     * Perform one request and decode the envelope.
     *
     * <p>The key travels in a header and nowhere else — never a query parameter,
     * which is written to access logs, kept in shell history and sent in Referer
     * headers.
     *
     * <p>{@code stream} chooses whether the body is read here or handed back as a
     * stream. It is not a convenience: for a followed log, reading the body to its
     * end is exactly the hang the streaming helpers exist to avoid.
     */
    private static Response send(String baseUrl, String key, String method, String path,
                                 Map<String, String> query, byte[] body, String contentType, boolean stream) {
        HttpRequest.Builder req = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl.replaceAll("/+$", "") + path + encodeQuery(query)))
                .header("Authorization", "Bearer " + key)
                .header("Accept", "application/json");
        if (contentType != null) {
            req.header("Content-Type", contentType);
        }
        if (body == null) {
            // HttpRequest.BodyPublishers.noBody() is right for a GET and for a
            // POST the server answers from its query parameters alone, such as
            // /complete. It is not right for an empty file: same bytes either way,
            // and the server does not distinguish them.
            req.method(method, HttpRequest.BodyPublishers.noBody());
        } else {
            req.method(method, HttpRequest.BodyPublishers.ofByteArray(body));
        }

        HttpResponse<InputStream> resp;
        try {
            resp = CLIENT.send(req.build(), HttpResponse.BodyHandlers.ofInputStream());
        } catch (IOException err) {
            // A transport failure — the deployment could not be reached at all. It
            // is the only case where there is no status to report, and it is
            // retryable in the sense that the same call may well work once the
            // network does.
            throw new AppLabException(0, "cannot reach " + baseUrl + ": " + err.getMessage(), true);
        } catch (InterruptedException err) {
            Thread.currentThread().interrupt();
            throw new AppLabException(0, "interrupted while calling " + baseUrl, true);
        }

        if (resp.statusCode() >= 400) {
            String text = readAll(resp.body());
            throw errorFrom(resp.statusCode(), text);
        }
        if (stream) {
            return new Response(resp.statusCode(), null, resp.body());
        }
        return new Response(resp.statusCode(), readAll(resp.body()), null);
    }

    /** Turn an HTTP failure into an AppLabException, reading the error envelope. */
    private static AppLabException errorFrom(int status, String text) {
        Matcher message = ERROR_FIELD.matcher(text);
        if (message.find()) {
            String value = stringFieldAfter(text, message.end());
            if (value != null) {
                return new AppLabException(status, value, RETRYABLE_FIELD.matcher(text).find());
            }
        }
        // A body that is not JSON — a proxy's HTML, a 413 from something in front
        // of the deployment — is reported by status and an excerpt, which is more
        // useful than a parse error.
        String excerpt = text.length() > 200 ? text.substring(0, 200) : text;
        return new AppLabException(status, "HTTP " + status + ": " + excerpt, status >= 500);
    }

    /**
     * Unwrap the {@code {"data": ...}} envelope every ordinary route answers with.
     *
     * <p>Found by locating the {@code "data":} key and taking the value that
     * follows, rather than by parsing the whole document — see {@link #jsonEscape}
     * for why this file exchanges JSON textually.
     */
    private static String unwrapData(String body) {
        if (body == null) {
            return null;
        }
        Matcher matcher = DATA_FIELD.matcher(body);
        if (!matcher.find()) {
            return body;
        }
        return valueAfter(body, matcher.end());
    }

    private static Map<String, String> uploadQuery(UploadOptions opts) {
        Map<String, String> query = new HashMap<>();
        query.put("message", opts.message);
        query.put("branch", opts.branch);
        // publish=false is the only thing that ever travels; see
        // UploadOptions.publish for why the default is left implicit.
        query.put("publish", opts.publish ? null : "false");
        return query;
    }

    private static UploadResult uploadResult(String body) {
        String data = unwrapData(body);
        if (data == null) {
            return new UploadResult(null, null, null, 0, 0L, null);
        }
        return new UploadResult(
                stringField(Pattern.compile("\"branch\"\\s*:\\s*\"([^\"]*)\""), data, 1),
                stringField(Pattern.compile("\"commit_sha\"\\s*:\\s*\"([^\"]*)\""), data, 1),
                stringField(Pattern.compile("\"message\"\\s*:\\s*\"([^\"]*)\""), data, 1),
                intField(Pattern.compile("\"files\"\\s*:\\s*(\\d+)"), data, 0),
                longField(Pattern.compile("\"bytes\"\\s*:\\s*(\\d+)"), data),
                stringField(Pattern.compile("\"stripped_root\"\\s*:\\s*\"([^\"]*)\""), data, 1));
    }

    /**
     * Read a body stream into memory. Only ever called on a response whose end is
     * certain — a finite one — never on a followed log.
     */
    private static String readAll(InputStream in) {
        try (InputStream body = in) {
            return new String(body.readAllBytes(), StandardCharsets.UTF_8);
        } catch (IOException err) {
            throw new AppLabException(0, "could not read the response: " + err.getMessage(), false);
        }
    }

    /**
     * Decode a body stream into lines lazily.
     *
     * <p>A {@link Stream} over an {@link Iterator} that pulls one line at a time
     * from a {@link BufferedReader}, so a caller that stops consuming (a
     * {@code limit}, a break out of a for-each) stops reading the socket rather
     * than draining a stream that may never end.
     *
     * <p>The terminator is not kept: it is whatever the server sent — usually
     * {@code \n}, possibly {@code \r\n} — and guessing wrong would mean a stray
     * carriage return in the caller's output. Each element is one line, so a
     * caller printing them adds its own newline.
     */
    private static Stream<String> readLines(Response resp) {
        BufferedReader reader = new BufferedReader(new InputStreamReader(resp.stream, StandardCharsets.UTF_8));
        Iterator<String> lines = new Iterator<String>() {
            private String next;
            private boolean done;

            private void advance() {
                if (next != null || done) {
                    return;
                }
                try {
                    next = reader.readLine();
                } catch (IOException err) {
                    done = true;
                    throw new UncheckedIOException(err);
                }
                if (next == null) {
                    done = true;
                }
            }

            @Override
            public boolean hasNext() {
                advance();
                return next != null;
            }

            @Override
            public String next() {
                advance();
                if (next == null) {
                    throw new NoSuchElementException();
                }
                String line = next;
                next = null;
                return line + "\n";
            }
        };
        return StreamSupport.stream(java.util.Spliterators.spliteratorUnknownSize(lines, 0), false)
                .onClose(() -> {
                    try {
                        reader.close();
                    } catch (IOException ignored) {
                        // Closing a stream that has already ended, or one the
                        // server closed, is not a failure worth reporting.
                    }
                });
    }

    // --- a minimal tar writer ----------------------------------------------

    /**
     * Walk a directory and write its tar stream.
     *
     * <p>Entries are walked in sorted order and mtimes are preserved so the same
     * tree produces the same archive — that determinism is what makes a re-push of
     * an unchanged directory a no-op commit. Each file's bytes are streamed into
     * the archive rather than buffered whole.
     */
    private static void writeTar(Path root, OutputStream out) throws IOException {
        List<Path> files = new ArrayList<>();
        Files.walkFileTree(root, new SimpleFileVisitor<Path>() {
            @Override
            public FileVisitResult preVisitDirectory(Path dir, BasicFileAttributes attrs) {
                if (!dir.equals(root) && SKIP_DIRS.contains(dir.getFileName().toString())) {
                    return FileVisitResult.SKIP_SUBTREE;
                }
                return FileVisitResult.CONTINUE;
            }

            @Override
            public FileVisitResult visitFile(Path file, BasicFileAttributes attrs) {
                if (attrs.isRegularFile() && !SKIP_DIRS.contains(file.getFileName().toString())) {
                    files.add(file);
                }
                // Symlinks are skipped, matching the Go client and the shell
                // script: an archive that follows a link out of the tree uploads
                // something the caller did not mean to send. attrs.isRegularFile()
                // is false for them, so they never reach the list above.
                return FileVisitResult.CONTINUE;
            }
        });
        Collections.sort(files);

        byte[] padding = new byte[512];
        for (Path file : files) {
            String name = root.relativize(file).toString().replace('\\', '/');
            if (name.getBytes(StandardCharsets.UTF_8).length > TAR_NAME_MAX) {
                throw new AppLabException(0, "path is too long for the archive format (100 bytes): " + name, false);
            }
            long size = Files.size(file);
            out.write(tarHeader(name, size, Files.getLastModifiedTime(file).toMillis() / 1000L));
            long written = 0;
            try (InputStream in = Files.newInputStream(file)) {
                byte[] buffer = new byte[8192];
                int read;
                while ((read = in.read(buffer)) >= 0) {
                    out.write(buffer, 0, read);
                    written += read;
                }
            }
            // The header declares the size read a moment ago. A file that changed
            // between then and now would otherwise land misaligned in the archive
            // and unpack as the wrong tree, so this refuses instead.
            if (written != size) {
                throw new IOException("file changed size while being packed (declared " + size
                        + ", read " + written + "): " + file);
            }
            int tail = (int) ((size % 512) == 0 ? 0 : 512 - (size % 512));
            if (tail > 0) {
                out.write(padding, 0, tail);
            }
        }
        out.write(padding, 0, 512); // two zero blocks end the archive
        out.write(padding, 0, 512);
    }

    /**
     * Build a 512-byte ustar header for one regular file.
     *
     * <p>Only what a source archive needs is implemented: no links, no PAX
     * extension headers. The "prefix" mechanism for paths longer than the
     * 100-byte name field is not implemented either; the caller refuses such a
     * path instead, because a header that silently truncates a name produces an
     * archive that unpacks to the wrong tree.
     */
    private static byte[] tarHeader(String name, long size, long mtimeSeconds) {
        byte[] header = new byte[512];
        put(header, 0, name);
        put(header, 100, "0000644");        // mode
        put(header, 108, "0000000");        // uid
        put(header, 116, "0000000");        // gid
        put(header, 124, Long.toOctalString(size));
        put(header, 136, Long.toOctalString(mtimeSeconds));
        put(header, 148, "        ");       // checksum is spaces while it is computed
        put(header, 156, "0");              // regular file
        put(header, 257, "ustar");          // magic: "ustar\0", zero-filled at 262
        put(header, 263, "00");             // version

        int sum = 0;
        for (byte b : header) {
            sum += b & 0xff;
        }
        // The checksum field is six octal digits, then NUL, then a space.
        put(header, 148, Long.toOctalString(sum));
        header[154] = 0;
        header[155] = ' ';
        return header;
    }

    /**
     * Write an ASCII/UTF-8 field into a tar header.
     *
     * <p>Numeric fields are octally encoded with a leading zero and a NUL
     * terminator, which is what {@code Long.toOctalString} plus the terminator
     * gives; the width is not padded, and readers accept that. It is the layouts
     * where the width <em>matters</em> — the checksum and the name — that are
     * handled explicitly above.
     */
    private static void put(byte[] header, int offset, String value) {
        byte[] bytes = value.getBytes(StandardCharsets.UTF_8);
        System.arraycopy(bytes, 0, header, offset, bytes.length);
    }

    // --- small textual readers ---------------------------------------------
    //
    // These exist because this SDK has no JSON dependency, for the same reason it
    // has no HTTP one. They are not a general parser: they find a named field in
    // the API's own envelope, which is a shape this repository controls. A value
    // that itself contains the text of a later key could in principle confuse
    // them, so they are pointed at the API's own responses and nothing else.

    /** The quoted string value of {@code "key":"..."}, or null. */
    private static String stringField(Pattern pattern, String body, int group) {
        if (body == null) {
            return null;
        }
        Matcher matcher = pattern.matcher(body);
        return matcher.find() ? matcher.group(group) : null;
    }

    /** The integer value of {@code "key":123}, or {@code fallback}. */
    private static int intField(Pattern pattern, String body, int fallback) {
        if (body == null) {
            return fallback;
        }
        Matcher matcher = pattern.matcher(body);
        return matcher.find() ? Integer.parseInt(matcher.group(1)) : fallback;
    }

    private static long longField(Pattern pattern, String body) {
        if (body == null) {
            return 0L;
        }
        Matcher matcher = pattern.matcher(body);
        return matcher.find() ? Long.parseLong(matcher.group(1)) : 0L;
    }

    /** Decode the JSON string starting at {@code from} (just past the colon). */
    private static String stringFieldAfter(String body, int from) {
        String value = valueAfter(body, from);
        if (value == null) {
            return null;
        }
        if (value.length() >= 2 && value.charAt(0) == '"' && value.endsWith("\"")) {
            value = value.substring(1, value.length() - 1);
        }
        return unescape(value);
    }

    /** The raw JSON value starting at {@code from} (just past a colon). */
    private static String valueAfter(String body, int from) {
        int start = from;
        while (start < body.length() && Character.isWhitespace(body.charAt(start))) {
            start++;
        }
        if (start >= body.length()) {
            return null;
        }
        if (body.charAt(start) == '"') {
            int end = start + 1;
            while (end < body.length()) {
                char c = body.charAt(end);
                if (c == '\\') {
                    end += 2;
                    continue;
                }
                if (c == '"') {
                    end++;
                    break;
                }
                end++;
            }
            return body.substring(start, Math.min(end, body.length()));
        }
        int end = start;
        int depth = 0;
        while (end < body.length()) {
            char c = body.charAt(end);
            if (c == '{' || c == '[') {
                depth++;
            } else if (c == '}' || c == ']') {
                if (depth == 0) {
                    break;
                }
                depth--;
            } else if (c == ',' && depth == 0) {
                break;
            }
            end++;
        }
        return body.substring(start, end).trim();
    }

    private static String unescape(String value) {
        if (value.indexOf('\\') < 0) {
            return value;
        }
        StringBuilder out = new StringBuilder(value.length());
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            if (c != '\\' || i + 1 >= value.length()) {
                out.append(c);
                continue;
            }
            char next = value.charAt(++i);
            switch (next) {
                case 'n': out.append('\n'); break;
                case 't': out.append('\t'); break;
                case 'r': out.append('\r'); break;
                case '"': out.append('"'); break;
                case '\\': out.append('\\'); break;
                case '/': out.append('/'); break;
                case 'b': out.append('\b'); break;
                case 'f': out.append('\f'); break;
                case 'u':
                    if (i + 4 < value.length()) {
                        out.append((char) Integer.parseInt(value.substring(i + 1, i + 5), 16));
                        i += 4;
                    }
                    break;
                default: out.append(next);
            }
        }
        return out.toString();
    }

    /** Escape a string for embedding in the one hand-built JSON body this file sends. */
    private static String jsonEscape(String value) {
        StringBuilder out = new StringBuilder(value.length() + 8);
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            switch (c) {
                case '"': out.append("\\\""); break;
                case '\\': out.append("\\\\"); break;
                case '\n': out.append("\\n"); break;
                case '\r': out.append("\\r"); break;
                case '\t': out.append("\\t"); break;
                default:
                    if (c < 0x20) {
                        out.append(String.format("\\u%04x", (int) c));
                    } else {
                        out.append(c);
                    }
            }
        }
        return out.toString();
    }

    /**
     * Percent-encode a query map, dropping absent values.
     *
     * <p>This is why the helpers build URLs by hand. The JDK ships no query-string
     * encoder ({@code URLEncoder} is HTML form encoding, where a space becomes
     * {@code +}), and the generator's {@code native} library has no URL builder to
     * borrow — so the few characters a message or a branch name can contain are
     * escaped here.
     */
    private static String encodeQuery(Map<String, String> query) {
        if (query == null || query.isEmpty()) {
            return "";
        }
        StringBuilder out = new StringBuilder();
        for (Map.Entry<String, String> entry : query.entrySet()) {
            if (entry.getValue() == null) {
                continue;
            }
            out.append(out.length() == 0 ? '?' : '&');
            out.append(URLEncoder.encode(entry.getKey(), StandardCharsets.UTF_8))
                    .append('=')
                    .append(URLEncoder.encode(entry.getValue(), StandardCharsets.UTF_8));
        }
        return out.toString();
    }
}
