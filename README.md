# Holloway — self-hosted webhook relay that never drops a webhook

Holloway is a **self-hosted webhook relay** for local development. It receives webhooks on your own VPS, writes every one to SQLite **before** delivery, and forwards them to your laptop — so a webhook that arrives while your machine is asleep, your dev server is restarting, or your tunnel is down is queued and delivered when you reconnect, not lost.

Think of it as the durable, own-your-data alternative to Smee and ngrok: one Go binary, your domain, your database, no account, no SaaS.

<p align="center">
  <img src="docs/dashboard.png" alt="Holloway dashboard showing persisted webhooks, local response bodies, and replay controls" width="900">
</p>

- **`holloway-server`** runs on a VPS and owns the public webhook URL.
- **`holloway`** runs on your machine and forwards webhooks to `localhost`.

```txt
[GitHub / Stripe] --POST /hook/{token}--> [holloway-server] --persist--> [SQLite]
                                                  |
                                          [WebSocket tunnel]
                                                  v
                                          [holloway client] --> localhost:3000
```

## Why Holloway?

Smee, ngrok, and similar tools are good at *live* forwarding. They fall down on the part that actually hurts in webhook work: the request that arrives while your laptop is closed, your server is mid-restart, or your connection blipped. With a plain tunnel, that delivery is gone, and you're stuck trying to get Stripe or GitHub to re-send it.

Holloway writes every webhook to SQLite the moment it arrives, before it tries to deliver. If your client is offline, the request stays queued and the provider gets a fast `202 Accepted` — no error, no retry storm, no duplicates. When you reconnect, pending webhooks drain automatically. If a payload exposed a bug, inspect it and **replay** it from the dashboard instead of trying to re-trigger the provider.

Use Holloway when losing a webhook costs more time than running a small relay.

## How it compares

| | Holloway | Smee / gosmee | ngrok | Hookdeck / Svix (SaaS) |
|---|:---:|:---:|:---:|:---:|
| Self-hosted, your domain | ✅ | ✅ | — | — |
| Persists webhooks (durable) | ✅ | — | — | ✅ |
| Queues while you're offline | ✅ | — | — | ✅ |
| Replay from a dashboard | ✅ | partial | ✅ | ✅ |
| Inspect headers / body | ✅ | — | ✅ | ✅ |
| Your data stays on your box | ✅ | ✅ (ephemeral) | — | — |
| Single binary, no account | ✅ | ✅ | — | — |
| Free | ✅ | ✅ | limited | paid |

Holloway is intentionally small. If you need fan-out to many destinations, payload transformation, provider signature verification, or team routing, a gateway like Hookdeck or Svix is the right tool. Holloway does one thing: get your webhooks to localhost, durably, and let you replay them.

## Features

- **Durable by default** — every webhook is written to SQLite before delivery is attempted.
- **Offline queue** — requests received while the client is disconnected stay pending and drain on reconnect.
- **Live response pass-through** — when your client is connected, the provider sees your local app's real status code.
- **Replay** — re-send any stored webhook to your local app from the dashboard or on connect.
- **Inspect & filter** — live dashboard with search by body, path, status class, and date.
- **One binary deploy** — Go server, SQLite storage, no external services. CSS/JS are vendored, not loaded from a CDN.
- **Token + tunnel-secret auth**, per-token rate limiting, and optional retention to bound disk usage.

## Delivery model

Holloway uses **hybrid delivery**:

- **Client connected** → the webhook is forwarded live and your local app's real status code and body are returned to the provider.
- **Client offline, or the local app is unreachable** → the webhook is persisted and the provider gets `202 Accepted`. It stays pending and is delivered when the client reconnects (and on a periodic drain while connected).

Because an unreachable client gets a `202` instead of an error, providers don't retry into duplicate rows. A `5xx` only reaches the provider when your local app is actually reachable and returned one.

## 60-second local setup

```sh
go build -o bin/holloway-server ./cmd/holloway-server
go build -o bin/holloway ./cmd/holloway
```

Start your local app on port 3000 — Holloway forwards incoming webhooks there.

Start the server:

```sh
HOLLOWAY_ADMIN_PASSWORD=admin \
HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET=local-dev-tunnel-secret \
./bin/holloway-server -addr :8080 -bootstrap-token local-dev-token
```

Connect the client:

```sh
./bin/holloway connect --server ws://localhost:8080 --token local-dev-token --secret local-dev-tunnel-secret --port 3000
```

Send a webhook:

```sh
curl -X POST http://localhost:8080/hook/local-dev-token -d '{}'
```

With the client connected you'll get your local app's response back. With no client connected you'll get `202 accepted` and the webhook waits in the queue.

Open the dashboard at `http://localhost:8080/dashboard`. It uses Basic Auth — the username is ignored; the password must match `HOLLOWAY_ADMIN_PASSWORD`.

## Server

```sh
holloway-server \
  -addr :8080 \
  -db holloway.db \
  -templates templates \
  -static static \
  -webhook-rate-limit 300 \
  -bootstrap-token local-dev-token \
  -bootstrap-tunnel-secret local-dev-tunnel-secret
```

Environment variables (flags take precedence):

- `HOLLOWAY_ADDR` — listen address, default `:8080`
- `HOLLOWAY_DB` — SQLite path, default `holloway.db`
- `HOLLOWAY_TEMPLATES` — template directory, default `templates`
- `HOLLOWAY_STATIC` — static directory, default `static`
- `HOLLOWAY_BOOTSTRAP_TOKEN` — optional initial token. No token is created by default.
- `HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET` — tunnel secret for the bootstrap token. Required when `HOLLOWAY_BOOTSTRAP_TOKEN` is set.
- `HOLLOWAY_ADMIN_PASSWORD` — required Basic Auth password for dashboard and token management
- `HOLLOWAY_ALLOW_INSECURE_ADMIN` — set to `true` only for local-only development without dashboard authentication
- `HOLLOWAY_WEBHOOK_RATE_LIMIT` — webhook requests per token per minute, default `300`. Requests over the limit return `429` before they are persisted.
- `HOLLOWAY_RETENTION_MAX_AGE` — delete webhooks older than this (e.g. `720h`). Disabled by default.
- `HOLLOWAY_RETENTION_MAX_ROWS` — keep at most this many webhooks per token. Disabled by default.

### Retention

By default Holloway keeps every webhook forever — durability is the point. For long-running servers, bound disk usage by setting either retention flag; a sweep runs hourly (and once at startup):

```sh
holloway-server -retention-max-age 720h -retention-max-rows 50000
```

Retention deletes rows but does not shrink the database file; reclaim space with a manual `VACUUM` if needed.

## Client

```sh
holloway connect --server wss://hooks.example.com --token tok_abc --secret tsec_abc --port 3000
```

Replay the last 10 stored webhooks after connecting:

```sh
holloway connect --server wss://hooks.example.com --token tok_abc --secret tsec_abc --port 3000 --replay 10
```

The client logs connection state and each forwarded webhook, and reconnects automatically with backoff.

The **webhook token** goes in provider URLs. The **tunnel secret** is only for the local client and is sent as a WebSocket `Authorization: Bearer ...` header. Holloway stores only a hash of the tunnel secret.

## API

Webhook ingress (the path after the token is preserved and forwarded):

```sh
curl -X POST https://hooks.example.com/hook/<token>/orders -d '{"id":1}'
```

Token creation:

```sh
curl -u ":$HOLLOWAY_ADMIN_PASSWORD" \
  -H "Origin: https://hooks.example.com" \
  -X POST https://hooks.example.com/tokens \
  -d "name=laptop"
```

The response includes the generated `tunnel_secret`. Save it when shown; Holloway stores only a hash. Admin POST requests require a same-origin `Origin` or `Referer` header.

Dashboard and admin routes:

```txt
GET  /dashboard
GET  /dashboard/events            # Server-Sent Events stream of live webhooks
GET  /dashboard/webhooks/:id
POST /dashboard/replay/:id
POST /tokens
POST /tokens/:id/delete
```

The webhook list is paginated and supports `q`, `path`, `status`, `from`, and `to` query parameters. Date filters use `YYYY-MM-DD`.

## Install from source

```sh
go install github.com/jolovicdev/holloway/cmd/holloway@v0.2.0
go install github.com/jolovicdev/holloway/cmd/holloway-server@v0.2.0
```

## Deploy with Docker Compose

Holloway ships with a Caddy sidecar that terminates TLS and obtains a free Let's Encrypt certificate automatically. Point your domain's DNS A/AAAA record at the host, make sure ports 80 and 443 are open, then:

```sh
export HOLLOWAY_DOMAIN='hooks.example.com'
export HOLLOWAY_ADMIN_PASSWORD='change-this'
export HOLLOWAY_BOOTSTRAP_TOKEN='use-a-long-random-token'
export HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET='use-another-long-random-token'
docker compose up -d --build
```

Caddy gets the certificate on the first request and reverse-proxies to Holloway, including the WebSocket tunnel. Holloway itself is not published to the host — only Caddy's `80`/`443` are exposed. Your client then connects over `wss://`:

```sh
holloway connect --server wss://hooks.example.com --token "$HOLLOWAY_BOOTSTRAP_TOKEN" --secret "$HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET" --port 3000
```

## Build releases

```sh
make build
```

Builds Linux, macOS, and Windows binaries (amd64 + arm64) under `bin/`.

## FAQ

**What happens to webhooks if my laptop is offline?**
They're persisted to SQLite and the provider gets `202 Accepted`. They stay pending and drain automatically when your client reconnects — nothing is lost.

**How is this different from Smee or gosmee?**
Smee-style relays forward live but don't persist; if no client is listening, the delivery is gone. Holloway writes every webhook to disk first and queues it, so you can go offline and replay later.

**How is it different from ngrok?**
ngrok is a general-purpose tunnel. Holloway is webhook-specific: it persists, queues, inspects, and replays, and it's self-hosted on your own domain with no account or bandwidth limits.

**How is it different from Hookdeck or Svix?**
Those are excellent hosted gateways. Holloway trades their fan-out, transformation, and team features for being a single self-hosted binary where the data never leaves your server.

**Does it verify provider signatures (e.g. GitHub HMAC)?**
No — Holloway forwards the raw request, headers included, so your local app verifies signatures exactly as it would in production.

## License

MIT.
