
# NGINX-DSTAT

## Requirements
- Go 1.21+
- nginx with stub_status module (almost always compiled in)
- Read access to /var/log/nginx/access.log

---

## 1. Apply nginx config

Edit your nginx config:

    nano /etc/nginx/nginx.conf

Add this inside your http {} block:

    server {
        listen 127.0.0.1:80;          # localhost only — never expose to the internet
        server_name localhost;
    
        # Allow access ONLY from loopback
        location /nginx_status {
            stub_status on;
            allow 127.0.0.1;
            deny  all;
            access_log off;
        }
    
        # Optionally block all other paths on this internal vhost
        location / {
            return 444;
        }
    }

    access_log /var/log/nginx/access.log combined;       # Find access log and replace it with this

Reload after applying the config:

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

Create nginx-dstat.service to /etc/systemd/system/:

    sudo nano /etc/systemd/system/nginx-dstat.service

nginx-dstat.service:

    [Unit]
    Description=Wireless Controller
    After=multi-user.target
    
    [Service]
    ExecStart=/root/nginx-dstat/nginx-dstat \
        -addr :8080 \
        -log /var/log/nginx/access.log \
        -status http://127.0.0.1/nginx_status \
        -interval 1300ms
    Restart=always
    User=root
    
    [Install]
    WantedBy=multi-user.target

Reload and start the service:

    sudo systemctl daemon-reload
    sudo systemctl restart nginx-dstat
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
