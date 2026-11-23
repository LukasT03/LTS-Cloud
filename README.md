# ☁️ LTS Cloud

Relay server for the [LTS Respooler](https://github.com/LukasT03/LTS-Respooler), running at `cloud.lts-design.com`. It lets [LTS Control](https://github.com/LukasT03/LTS-Control-iOS) reach a board from outside the local network. Locally, the apps talk to the board directly over WLAN or Bluetooth.

The server does not interpret the protocol. It passes the board's JSON status to the apps and the apps' `CMD`/`SET` messages to the board, the same JSON as over BLE.

## Authentication

Every board creates a random **ID** (16 hex characters) and **secret** (32 hex characters) on first boot and keeps them in NVS. The apps get both over BLE or the local network. Every request sends

```
Authorization: Board <id>:<secret>
```

The first board connection with an unknown ID registers it. Apps can only use boards that are already registered. If a board loses its NVS (full flash erase), it creates a new pair and the apps pick it up on the next local connection.

## API

| Endpoint | Who | What |
|---|---|---|
| `GET /v1/board` (WebSocket) | Board | Board sends status packets, receives `CMD`/`SET` messages. A new connection replaces the old one. |
| `GET /v1/app` (WebSocket) | App | First message is the full last status. Then every status packet as the board sent it, plus `{"ONLINE":true/false}`. `CMD`/`SET` messages go to the board, `{"ERR":"OFFLINE"}` or `{"ERR":"INVALID"}` come back if they cannot. |
| `GET /v1/status` | App | Full last status as JSON. |
| `POST /v1/command` | App, Live Activity buttons | Body is a `CMD`/`SET` message. `202` sent, `400` invalid, `503` board offline. |
| `GET /healthz` | Monitoring | `ok` |

Status packets from the board are merged, so the last status always has every field. The server adds two fields of its own: `ONLINE` (board connected right now) and `SEEN` (Unix time of the last packet). Messages are limited to 4 KB. The server pings every 30 seconds.

## Data

Per board, the server stores:

- the ID,
- a SHA-256 hash of the secret,
- the last status (state, progress, settings: the same fields the board sends over BLE),
- the time of the last connection.

Boards that have not connected for 180 days are deleted. There are no accounts, names or email addresses. The server does not log IP addresses, and the nginx configuration turns off its access log.

## Development

```bash
go test -race ./...
```

```bash
go run .
```

Settings come from the environment: `LISTEN_ADDR` (default `127.0.0.1:8080`) and `DB_PATH` (default `cloud.db`).

## Deployment

Build for the VPS (adjust `GOARCH` to the server, `uname -m`: `x86_64` is `amd64`, `aarch64` is `arm64`):

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o lts-cloud .
```

On the server:

1. Copy `lts-cloud` to `/usr/local/bin/`.
2. Copy `deploy/lts-cloud.service` to `/etc/systemd/system/`, then `systemctl enable --now lts-cloud`. The database lives in `/var/lib/lts-cloud/`.
3. Copy `deploy/nginx.conf` to `/etc/nginx/sites-available/lts-cloud`, link it to `sites-enabled`, get a certificate with `certbot --nginx -d cloud.lts-design.com` and reload nginx.

The domain is built into the board firmware and must never change. Move the server by pointing the DNS record somewhere else.
