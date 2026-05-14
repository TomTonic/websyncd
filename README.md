# websyncd

websyncd is a small Go daemon that keeps a local file in sync with a remote HTTP resource using HEAD/GET, optional webhooks/SSE, and robust polling fallback.

## Features

- **Bandwidth-efficient polling** — issues a `HEAD` request first; only fetches the full body with `GET` when the resource has actually changed (using `ETag` / `Last-Modified` conditional headers).
- **Webhook trigger** — listens for `POST /` on a configurable address so external systems can push an immediate sync without waiting for the next poll tick.
- **Resource event stream (SSE) trigger** — connects to a Server-Sent Events endpoint and triggers a sync on every event, with automatic reconnection on failure.
- **HTTP/3 Auto-Upgrade** — enabled by default; the first request to an origin uses TCP and the `Alt-Svc` response header is parsed. If the server advertises `h3`, subsequent requests automatically use HTTP/3 (QUIC). A per-origin cooldown prevents repeated QUIC attempts when UDP is blocked. Set `ENABLE_HTTP3=false` to opt out entirely.
- **Atomic file writes** — writes to a temporary file in the same directory, then renames it into place, so readers never see a partial file.
- **Instance locking** — uses a PID/timestamp lock file in `$TMPDIR` (keyed by a SHA-256 of the resource URL + output path) to prevent two daemons from racing over the same file. Stale locks from crashed processes are cleared automatically after a configurable TTL.
- **Graceful shutdown** — handles `SIGINT` / `SIGTERM` and stops all goroutines cleanly.
- **Operational logging** — emits detailed sync diagnostics: trigger source, download decision, local replace/skip decision, protocol (`HTTP`/`HTTPS` + HTTP version), transfer rate, size delta, and freshness delta.
- **Heartbeat endpoints** — optional probe endpoints for orchestration: `GET /healthz` (liveness from internal heartbeat, independent of poll interval or upstream availability) and `GET /readyz` (readiness after first successful upstream response).
- **Container-friendly configuration** — all settings are read from environment variables; no config files required.

## Usage

### Build

```sh
go build -o websyncd ./cmd/websyncd
```

### Run

```sh
RESOURCE_URL=https://example.com/data.json \
OUTPUT_PATH=/var/data/data.json \
./websyncd
```

### Docker

Pull the published image from GHCR:

```sh
docker pull ghcr.io/tomtonic/websyncd:latest
```

Run a single sync service:

```sh
docker run --rm \
  -e RESOURCE_URL=https://example.com/data.json \
  -e OUTPUT_PATH=/data/data.json \
  -e MAX_DOWNLOAD_BYTES=10485760 \
  -e HEARTBEAT_ADDR=:8081 \
  -v "$(pwd)/data:/data" \
  ghcr.io/tomtonic/websyncd:latest
```

Optional: add a Docker healthcheck against the heartbeat endpoint:

```sh
--health-cmd='curl -fS http://127.0.0.1:8081/healthz >/dev/null 2>&1 || exit 1' \
--health-interval=30s --health-timeout=5s --health-retries=3
```

An example `docker-compose.yaml` is included that runs two services writing into the same local `./data` directory:

- `adguard-filter-updater` downloads `https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt` into `./data/adguard-filter.txt` and keeps it in sync with the online version.
- `stevenblack-hosts-updater` downloads `https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts` into `./data/stevenblack-hosts.txt` and keeps it in sync with the online version.

Start both services:

```sh
docker compose up -d
```

## CI and Release Workflows

- **CI** (`.github/workflows/ci.yaml`) runs build, test, and `golangci-lint` (`latest`) on GitHub Actions and uploads Linux binary artifacts for amd64, arm64, armv7, and armv6.
- **Release** (`.github/workflows/release.yaml`) runs on `v*` tag pushes, waits for successful CI completion for the same commit, then packages those CI artifacts into a multi-arch Docker image and pushes it to GHCR.

The published `ghcr.io/tomtonic/websyncd:latest` image is multi-arch (linux/amd64, linux/arm64, linux/arm/v7, linux/arm/v6).

### Environment Variables

| Variable             | Required | Default  | Description |
|----------------------|----------|----------|-------------|
| `RESOURCE_URL`       | yes      | —        | URL of the remote resource to sync. Must be a valid `http://` or `https://` URL.     |
| `RESOURCE_EVENT_URL` | no       | `—`      | If set, connect to this Server-Sent Events (SSE) stream and trigger a sync attempt on each event. Must be a valid `http://` or `https://` URL. |
| `OUTPUT_PATH`        | yes      | —        | Local file path to write the resource to. Parent directories are created automatically. |
| `OUTPUT_FILE_ATTRIBUTES` | no   | `—`      | Optional output file attributes in format `uid:gid:mode` (for example `1000:1000:0644`). When set, replacements are written with exactly these owner/group/permissions. When unset and target exists, owner/group/permissions are inherited from the existing file. When unset and target does not exist, owner/group follow process defaults and permissions default to `ugo+r` (`0644`). |
| `POLL_INTERVAL`      | no       | `1h`    | How often to poll the remote resource (Go duration string, e.g. `30s`, `5m`). The minimum value is 5 seconds. |
| `WEBHOOK_ADDR`       | no       | `—`      | If set, start an HTTP webhook server that accepts `POST /` to trigger an immediate sync attempt (e.g. `127.0.0.1:8080` or `:9000`). Must be `host:port` where `host` is an IP address, hostname, or empty and `port` is a numeric port. |
| `HEARTBEAT_ADDR`     | no       | `—`      | If set, start HTTP probe endpoints at this address (e.g. `127.0.0.1:8081` or `:8081`). `GET /healthz` reports liveness (internal daemon heartbeat), `GET /readyz` reports readiness (first successful upstream response). See Heartbeat endpoints section for status codes. Must be `host:port` where `host` is an IP address, hostname, or empty and `port` is a numeric port. |
| `HTTP_TIMEOUT`       | no       | `30s`    | Timeout for individual HTTP requests. |
| `LOCK_TTL`           | no       | `5m`     | How long before a lock from a previous (crashed) instance is considered stale. |
| `ENABLE_HTTP3`       | no       | `true`   | Set to `false` to disable HTTP/3 Auto-Upgrade entirely (useful when QUIC is blocked or causes problems). When `true` (default), the first request to an origin uses TCP; if the server's `Alt-Svc` response header advertises `h3`, subsequent requests use HTTP/3 (QUIC) automatically. A per-origin cooldown of ~7 minutes prevents repeated QUIC retries after a failure. |
| `DOWNLOAD_PROGRESS_INTERVAL` | no | `5s` | How often to emit progress log messages during long-running downloads (Go duration string, e.g. `5s`, `1m`, `500ms`). |
| `MAX_DOWNLOAD_BYTES` | no       | `0`      | Maximum allowed size for a downloaded response body in bytes (non-negative integer). Use a value >0 to protect against runaway responses; `0` means no limit. |

### Semantics and Status Codes of HTTP Probing Endpoints

- `/healthz` (Liveness)
  - `200 OK`: Internal daemon loop is responsive (heartbeat firing every 15s).
  - `500 Internal Server Error`: Loop heartbeat stalled (reason: `loop_heartbeat_stalled` or `loop_heartbeat_missing`).
  
- `/readyz` (Readiness)
  - `200 OK`: At least one successful upstream response received and failure rate acceptable.
  - `500 Internal Server Error`: Resource not accessible (reason: `resource_not_accessible` if no success yet, `resource_recently_unavailable` if failure rate > 50%).
  - `503 Service Unavailable`: Initial sync pending (never attempted).

### Examples

Poll every 10 seconds and trigger via webhook on port 9000:

```sh
RESOURCE_URL=https://cdn.example.com/config.yaml \
OUTPUT_PATH=/etc/myapp/config.yaml \
POLL_INTERVAL=10s \
WEBHOOK_ADDR=:9000 \
./websyncd
```

Use SSE for push-driven updates with a 5-minute polling fallback:

```sh
RESOURCE_URL=https://api.example.com/data.json \
OUTPUT_PATH=/tmp/data.json \
POLL_INTERVAL=5m \
RESOURCE_EVENT_URL=https://api.example.com/events \
./websyncd
```

Trigger an immediate sync manually (when webhook is enabled):

```sh
curl -X POST http://localhost:8080/
```

Heartbeat endpoint check (when enabled):

```sh
curl http://127.0.0.1:8081/healthz
```

Readiness endpoint check (when enabled):

```sh
curl http://127.0.0.1:8081/readyz
```

## Design Decisions

### HEAD-before-GET
Every sync cycle starts with a conditional `HEAD` request carrying `If-None-Match` (ETag) and `If-Modified-Since` headers. A `GET` is only issued when the server indicates the resource has changed (or when the server does not support `HEAD`). This avoids transferring the full body on every poll tick.

### Atomic writes
The downloaded body is written to a temporary file (`.websyncd-*`) in the same directory as the target, then moved into place with `os.Rename`. Because rename is atomic on POSIX systems (same filesystem), consumers reading the file will always see either the old complete version or the new complete version — never a partial write.

When `OUTPUT_FILE_ATTRIBUTES` is not configured, replacement files inherit owner/group/permissions from the existing target file. On first write (no existing target), default process owner/group are used and permissions default to `0644` (`ugo+r`).

### Trigger coalescing
All sync sources (poll timer, webhook, SSE) feed into a single buffered channel of capacity 1. If multiple triggers arrive while a sync is already in progress, they collapse into a single pending re-check, preventing redundant back-to-back fetches.

### Log output details
Each sync cycle produces structured log lines that explain:

- **Why sync started**: `trigger_source` (for example `startup`, `poll`, `webhook`, `sse`) and whether extra triggers were coalesced.
- **Why download ran or was skipped**: based on HEAD/GET outcomes (`304`, matching validators, fallback from HEAD to GET, etc.).
- **Why local file replacement ran or was skipped**: replacement is skipped when downloaded bytes are identical to the existing output (useful after restarts).
- **Which protocol was used**: `HTTP` vs `HTTPS`, plus HTTP version (`HTTP/1.1`, `HTTP/2`, `HTTP/3`).
- **Transfer metrics**: bytes transferred, duration, and effective throughput.
- **Version deltas**: previous size, new size, signed size delta, and freshness delta from `Last-Modified` when available.

### Instance locking
The lock file path is derived from a SHA-256 digest of `RESOURCE_URL + "|" + OUTPUT_PATH`. This allows multiple websyncd instances to run concurrently for different resource/output combinations on the same host, while still preventing duplicate instances for the same pair. If a lock file is found that is older than `LOCK_TTL`, it is treated as stale (the previous process likely crashed) and removed.

### HTTP/3 Auto-Upgrade (Alt-Svc)
HTTP/3 is enabled by default. The mechanism is based on the server-advertised `Alt-Svc` HTTP response header:

1. The first request to an origin always goes over TCP (HTTP/1.1 or HTTP/2).
2. If the response includes an `Alt-Svc` header with an `h3` token (e.g. `h3=":443"; ma=86400`), the origin is promoted in an in-memory cache. The `ma` (max-age) parameter controls how long the entry is valid; if absent a default TTL of 24 hours is used.
3. All subsequent requests to that origin use HTTP/3 (QUIC) directly.
4. If an HTTP/3 attempt fails, a 7-minute cooldown is recorded for that origin so that UDP-blocked networks are not flooded with failing QUIC attempts.

Set `ENABLE_HTTP3=false` to disable the feature entirely and always use TCP.

## License

See [LICENSE](LICENSE).
