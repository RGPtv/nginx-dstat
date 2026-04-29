
# NGINX-DSTAT

## Requirements
- Go 1.21+
- nginx with stub_status module (almost always compiled in)
- Read access to /var/log/nginx/access.log

---

## 1. Apply nginx config

Copy the relevant blocks from nginx-snippet.conf into your nginx config,
then reload:

    sudo nginx -t && sudo nginx -s reload

Verify stub_status is working:

    curl http://127.0.0.1/nginx_status

---

## 2. Build the Go server

    cd nginx-dstat
    go mod tidy
    go build -o nginx-dstat .

---

## 3. Run it

    # Basic — defaults work for most setups
    sudo ./nginx-dstat

    # Custom paths / ports
    sudo ./nginx-dstat \
      -addr    :8765 \
      -log     /var/log/nginx/access.log \
      -status  http://127.0.0.1/nginx_status \
      -interval 1500ms \
      -cors    "*"

    # sudo is needed to read /var/log/nginx/access.log.
    # Alternatively: add your user to the adm group:
    #   sudo usermod -aG adm $USER

Open your browser at:  http://YOUR-SERVER-IP:8765

---

## 4. Run as a systemd service (optional)

Copy nginx-dstat.service to /etc/systemd/system/, then:

    sudo systemctl daemon-reload
    sudo systemctl enable --now nginx-dstat
    sudo systemctl status nginx-dstat

---

## Flags reference

| Flag        | Default                          | Description                      |
|-------------|----------------------------------|----------------------------------|
| -addr       | :8765                            | Dashboard listen address         |
| -log        | /var/log/nginx/access.log        | nginx access log path            |
| -status     | http://127.0.0.1/nginx_status    | nginx stub_status URL            |
| -interval   | 1500ms                           | WebSocket push interval          |
| -cors       | *                                | CORS origin header               |

---

## Architecture

    nginx access.log  ──tail──►  Go tailer goroutine
                                       │
                                       ▼
    nginx stub_status ──poll──►  Aggregator goroutine (per interval)
                                       │
                                       ▼
                                  WebSocket Hub  ──broadcast──►  Browser dashboard
