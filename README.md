# websyncd

websyncd is a small Go daemon that keeps a local file in sync with a remote HTTP resource using HEAD/GET, optional webhooks/SSE, and robust polling fallback.

## Features

- **Bandwidth-efficient polling** — issues a `HEAD` request first; only fetches the full body with `GET` when the resource has actually changed (using `ETag` / `Last-Modified` conditional headers).
- **Webhook trigger** — listens for `POST /` on a configurable address so external systems can push an immediate sync without waiting for the next poll tick.
- **SSE trigger** — connects to a Server-Sent Events endpoint and triggers a sync on every event, with automatic reconnection on failure.
- **HTTP/3 (QUIC) support** — optionally uses HTTP/3 with transparent fallback to HTTP/1.1+HTTP/2 for bodyless requests (HEAD) if the server does not support QUIC.
- **Atomic file writes** — writes to a temporary file in the same directory, then renames it into place, so readers never see a partial file.
- **Instance locking** — uses a PID/timestamp lock file in `$TMPDIR` (keyed by a SHA-256 of the resource URL + output path) to prevent two daemons from racing over the same file. Stale locks from crashed processes are cleared automatically after a configurable TTL.
- **Graceful shutdown** — handles `SIGINT` / `SIGTERM` and stops all goroutines cleanly.
- **Operational logging** — emits startup, trigger, sync success/failure, SSE/webhook lifecycle, periodic heartbeat, and shutdown logs.
- **Heartbeat endpoint** — optional `GET /healthz` endpoint for liveness monitoring (ideal for Docker `healthcheck`).
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
  -e ENABLE_HEARTBEAT=true \
  -e HEARTBEAT_ADDR=:8081 \
  -v "$(pwd)/data:/data" \
  ghcr.io/tomtonic/websyncd:latest
```

Optional: add a Docker healthcheck against the heartbeat endpoint:

```sh
--health-cmd='wget -q -O - http://127.0.0.1:8081/healthz >/dev/null 2>&1 || exit 1' \
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

| Variable         | Required | Default  | Description |
|------------------|----------|----------|-------------|
| `RESOURCE_URL`   | yes      | —        | URL of the remote resource to sync. |
| `OUTPUT_PATH`    | yes      | —        | Local file path to write the resource to. Parent directories are created automatically. |
| `POLL_INTERVAL`  | no       | `30m`    | How often to poll the remote resource (Go duration string, e.g. `30s`, `5m`). |
| `HTTP_TIMEOUT`   | no       | `30s`    | Timeout for individual HTTP requests. |
| `LOCK_TTL`       | no       | `5m`     | How long before a lock from a previous (crashed) instance is considered stale. |
| `HEARTBEAT_INTERVAL` | no    | `5m`     | Interval for periodic “still alive” heartbeat log messages. |
| `ENABLE_WEBHOOK` | no       | `false`  | When `true`, start an HTTP server that accepts `POST /` to trigger an immediate sync. |
| `WEBHOOK_ADDR`   | no       | `:8080`  | Address the webhook server listens on (e.g. `127.0.0.1:9000`). |
| `ENABLE_SSE`     | no       | `false`  | When `true`, connect to `SSE_URL` and trigger a sync on each event. |
| `SSE_URL`        | cond.    | —        | Required when `ENABLE_SSE=true`. URL of the SSE stream. |
| `ENABLE_HTTP3`   | no       | `false`  | When `true`, use HTTP/3 (QUIC) as the primary transport with automatic fallback. |
| `ENABLE_HEARTBEAT` | no     | `false`  | When `true`, start an HTTP heartbeat endpoint for liveness checks. |
| `HEARTBEAT_ADDR` | no       | `:8081`  | Address the heartbeat server listens on (e.g. `127.0.0.1:8081`). |

### Examples

Poll every 10 seconds and trigger via webhook on port 9000:

```sh
RESOURCE_URL=https://cdn.example.com/config.yaml \
OUTPUT_PATH=/etc/myapp/config.yaml \
POLL_INTERVAL=10s \
ENABLE_WEBHOOK=true \
WEBHOOK_ADDR=:9000 \
./websyncd
```

Use SSE for push-driven updates with a 5-minute polling fallback:

```sh
RESOURCE_URL=https://api.example.com/data.json \
OUTPUT_PATH=/tmp/data.json \
POLL_INTERVAL=5m \
ENABLE_SSE=true \
SSE_URL=https://api.example.com/events \
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

## Design Decisions

### HEAD-before-GET
Every sync cycle starts with a conditional `HEAD` request carrying `If-None-Match` (ETag) and `If-Modified-Since` headers. A `GET` is only issued when the server indicates the resource has changed (or when the server does not support `HEAD`). This avoids transferring the full body on every poll tick.

### Atomic writes
The downloaded body is written to a temporary file (`.websyncd-*`) in the same directory as the target, then moved into place with `os.Rename`. Because rename is atomic on POSIX systems (same filesystem), consumers reading the file will always see either the old complete version or the new complete version — never a partial write.

### Trigger coalescing
All sync sources (poll timer, webhook, SSE) feed into a single buffered channel of capacity 1. If multiple triggers arrive while a sync is already in progress, they collapse into a single pending re-check, preventing redundant back-to-back fetches.

### Instance locking
The lock file path is derived from a SHA-256 digest of `RESOURCE_URL + "|" + OUTPUT_PATH`. This allows multiple websyncd instances to run concurrently for different resource/output combinations on the same host, while still preventing duplicate instances for the same pair. If a lock file is found that is older than `LOCK_TTL`, it is treated as stale (the previous process likely crashed) and removed.

### HTTP/3 fallback
When `ENABLE_HTTP3=true`, requests are attempted over QUIC first. For requests without a body (all HEAD requests, and GET requests where the body has not yet been read), a transparent retry over HTTP/1.1 is performed if the QUIC attempt fails — for example, because the server does not advertise QUIC support.

## License

See [LICENSE](LICENSE).
