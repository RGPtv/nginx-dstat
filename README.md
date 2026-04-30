# NGINX-DSTAT

A lightweight real-time nginx traffic dashboard — a Go backend tails your access log and streams live metrics to a browser via WebSocket.

## Requirements

- Go 1.21+
- nginx with `stub_status` module (compiled in by default on most distros)
- Read access to `/var/log/nginx/access.log`

---

## 1. Apply nginx config

Edit your nginx config:

```sh
nano /etc/nginx/nginx.conf
```

Add this inside your `http {}` block:

```nginx
server {
    listen 127.0.0.1:80;   # localhost only — never expose to the internet
    server_name localhost;

    location /nginx_status {
        stub_status on;
        allow 127.0.0.1;
        deny  all;
        access_log off;
    }

    # Block all other paths on this internal vhost
    location / {
        return 444;
    }
}

access_log /var/log/nginx/access.log combined;  # ensure combined format
```

Reload after applying the config:

```sh
sudo nginx -t && sudo nginx -s reload
```

Verify stub_status is working:

```sh
curl http://127.0.0.1/nginx_status
```

Test log access:

```sh
tail -f /var/log/nginx/access.log
```

---

## 2. Build the Go server

```sh
cd nginx-dstat
go mod tidy
go build -ldflags "-X main.version=v1.0.0" -o nginx-dstat .
```

> Omit `-ldflags` if you don't need a version string — the binary will report `dev`.

---

## 3. Run it

```sh
# Basic — defaults work for most setups
sudo ./nginx-dstat

# Custom paths / ports
sudo ./nginx-dstat \
  -addr            :8765 \
  -log             /var/log/nginx/access.log \
  -status          http://127.0.0.1/nginx_status \
  -interval        1500ms \
  -status-interval 2s \
  -cors            "*" \
  -max-paths       10000
```

> `sudo` is needed to read `/var/log/nginx/access.log`.  
> Alternatively, add your user to the `adm` group and restart your session:
> ```sh
> sudo usermod -aG adm $USER
> ```

Open your browser at: **http://YOUR-SERVER-IP:8765**

---

## 4. Run as a systemd service (optional)

Create the unit file:

```sh
sudo nano /etc/systemd/system/nginx-dstat.service
```

```ini
[Unit]
Description=NGINX DSTAT
After=multi-user.target

[Service]
ExecStart=/root/nginx-dstat/nginx-dstat \
    -addr            :8765 \
    -log             /var/log/nginx/access.log \
    -status          http://127.0.0.1/nginx_status \
    -interval        1300ms \
    -status-interval 2s \
    -from-start
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now nginx-dstat
sudo systemctl status nginx-dstat
```

---

## Flags reference

| Flag               | Default                         | Description                                                                 |
|--------------------|---------------------------------|-----------------------------------------------------------------------------|
| `-addr`            | `:8765`                         | Dashboard listen address                                                    |
| `-log`             | `/var/log/nginx/access.log`     | nginx access log path                                                       |
| `-status`          | `http://127.0.0.1/nginx_status` | nginx stub_status URL                                                       |
| `-interval`        | `1500ms`                        | WebSocket push interval (how often metrics are broadcast to clients)        |
| `-status-interval` | `2s`                            | How often to poll nginx stub_status                                         |
| `-cors`            | `*`                             | `Access-Control-Allow-Origin` header value                                  |
| `-from-start`      | `false`                         | Tail the log from the beginning instead of the end (useful for replaying)  |
| `-max-paths`       | `10000`                         | Max unique paths tracked before the least-hit half are pruned               |

---

## Dashboard features

- **req/s** — requests per second computed from the current push interval window
- **Active / Reading / Writing / Waiting** — live nginx connection counters from `stub_status`
- **Bandwidth** — KB sent per push interval
- **Error rate** — percentage of 4xx + 5xx responses in the current window
- **Dropped lines** — log lines skipped when the aggregator channel is full (indicates very high traffic)
- **Response code chart** — rolling 40-point sparklines for 2xx / 3xx / 4xx / 5xx
- **Endpoint doughnut** — cumulative hit distribution across all tracked paths (up to `-max-paths`)
- **Top 10 paths** — ranked path list with total hit counts since startup
- **Live log tail** — last 50 requests in reverse-chronological order with IP, method, status, path, and size

---

## Architecture

```
nginx access.log  ──tail──►  tailer goroutine  ──chan LogLine (4096)──►  aggregator goroutine
                                                                               │
nginx stub_status ──poll──►  StubFetcher goroutine (cached, RW-locked)  ──►  │
                                                                               │
                                                                         per -interval tick
                                                                               │
                                                                               ▼
                                                                        WebSocket Hub
                                                                               │
                                                                        broadcast JSON
                                                                               │
                                                                               ▼
                                                                      Browser dashboard
```

Key design notes:

- **Tailer** polls the file every 200 ms and detects log rotation by comparing the file size to the last known offset.
- **Aggregator** maintains a fixed-size ring buffer (50 entries) for the live tail and a cumulative `map[path]count` for the endpoint chart. When the map exceeds `-max-paths` entries, the bottom half by hit count is pruned.
- **Hub** uses a per-client write queue (depth 8). Slow clients are dropped rather than blocking the broadcast path.
- **StubFetcher** runs in its own goroutine and caches the last successful `stub_status` response behind an `RWMutex`, decoupling the HTTP poll latency from the aggregator tick.
- All goroutines respect a shared `context.Context` and shut down cleanly on `SIGINT` / `SIGTERM`.

---

## Health check

```sh
curl http://localhost:8765/health
# {"status":"ok","version":"v1.0.0"}
```
