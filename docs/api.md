# HTTP API

[Documentation](README.md) · [Project home](../README.md)

The API has no authentication. Read [network access and trust](usage.md#network-access-and-trust) before exposing it beyond your computer.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/health` | Liveness. |
| `GET` | `/api/state` | Current snapshot. |
| `GET` | `/api/events` | Complete initial snapshot, then item patches; missed updates are replaced with a complete snapshot. |
| `GET` | `/api/logs` | Recent service logs. Pass the returned `session` and `next` as `session` and `after` for new entries; `reset` replaces prior history after a restart or retention gap. |
| `POST` | `/api/downloads` | `{"urls": "…", "password": "…"}` — newline-separated or an array. |
| `GET` | `/api/settings` | Current runtime settings, including configured proxy endpoints and discovery feeds. |
| `POST` | `/api/settings` | Any of `{"concurrency": n, "streams": n, "paused": bool, "speedLimit": n, "downloadDir": "…", "proxies": bool, "proxyEndpoints": ["…"], "proxyFeeds": ["…"]}` — each optional, so a request carries only what changed. |
| `GET` | `/api/proxies` | Proxy measurements and feed status. Query: `site` (`keep2share`, the default, `fileboom`, `emload`, `mega`, or `cloudflare`), `offset`, `limit` (1–200, default 50), `search`, `status` (`all`, `available`, `active`, `busy`, `cooling`, `untested`, `finishing`), `sort` (`score`, `throughput`, `success`, `address`). |
| `POST` | `/api/clear` | Forget finished jobs. |
| `POST` | `/api/jobs/{id}/cancel` · `/retry` | Whole job. |
| `POST` | `/api/jobs/{id}/limits` | `{"fileLimit": n, "streamLimit": n, "speedLimit": n}`, each optional: replaces the job's own limits beneath the queue's; a field left out has none. `fileLimit` 0 holds its new downloads. |
| `DELETE` | `/api/jobs/{id}` | Cancel and forget. |
| `POST` | `/api/jobs/{id}/items/{itemId}/cancel` · `/retry` | One file. |

Settings updates are validated together: an invalid field leaves the
current settings unchanged, including when opening the proxy database fails.
Proxy inventory is paged separately from queue snapshots; endpoint passwords
are redacted there and only included when explicitly fetching settings for
editing. In an SSE job marked `patch`, merge items by
ID and retain unmentioned items; otherwise replace its item list. Every
frame contains the complete job list and current aggregates.

Oversized JSON requests return `413`; malformed JSON returns `400`. Unknown
API routes return `404`, and a known route with an unsupported method returns
`405`. Ordinary HTTP requests have 30-second read/write deadlines. Event
streams use their own per-write deadline and remain open while idle.

```bash
curl -X POST localhost:8080/api/downloads \
  -H 'Content-Type: application/json' \
  -d '{"urls":"https://pixeldrain.com/l/<id>"}'
```
