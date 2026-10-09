# mesh-shop (s3)

Deployed with [ox](https://deploywithox.com): deploy a repo to your own server with one command, no Docker. [Docs](https://deploywithox.com/docs) · [Stack guides](https://deploywithox.com/docs/guides)

**Live demo:** https://mesh-shop.s3.zoo.sorv.dev

> **Role in the zoo:** project `mesh-shop` of [oxzoo-live](https://github.com/saurav-codes/oxzoo-live-control/blob/main/zoo/README.md#projects), deployed with [ox](https://deploywithox.com) on server s3 at https://mesh-shop.s3.zoo.sorv.dev. The contract it follows is [DESIGN.md](https://github.com/saurav-codes/oxzoo-live-control/blob/main/zoo/DESIGN.md).

A multi-process monorepo in one ox project: a Go gateway as `[app]`, a static shop page
built with Bun, two port workers (`orders` on Bun, `stock` on Python via uv), a plain
worker (`fulfiller` on Node), plus postgres and a private redis. It starts the
`shop-order` chain (P4) and signs calls to catalog-api (s2) and rails-queue (s1).

```
browser -> Caddy -> web/dist (static, spa)
                 -> /api, /_zoo -> gateway (Go, [app])
gateway -> orders (ORDERS_URL)  -> signed GET ${CATALOG_URL}/api/items/<sku> (INTERNAL_TOKEN, X-Zoo-Trace)
                                -> stock (STOCK_URL) POST /reserve, PG row lock (select ... for update)
                                -> order in PG, LPUSH mesh-shop:orders
fulfiller: BRPOP (5 s) -> mark fulfilled -> signed POST ${RAILS_URL}/webhooks/zoo (WEBHOOK_SECRET)
```

## What it proves

- One repo, four runtimes (Go 1.27, Bun 1.3, Python 3.13 with uv 0.11, Node 24), pinned in `[tools]`.
- `port = true` workers with `health`, and ox wiring `ORDERS_URL` and `STOCK_URL` into the gateway.
- A plain worker with no port, alive only through its Redis heartbeat.
- `[static]` with `spa` and `api` prefixes routing `/api` and `/_zoo` to the app.
- `[build] migrate` (one schema for the project, in `stock/migrate.py`).
- Shared postgres and private redis, used from three languages.
- Two shared secrets across three projects, proven with signed `/_zoo/verify` calls.

## Endpoints

| Path | What |
|------|------|
| `GET /` | shop page: stock, order button, live chain steps, recent orders |
| `GET /api/stock`, `GET /api/orders` | stock levels, last 20 orders |
| `POST /api/orders` `{"sku","trace"}` | place an order (runs the chain, same rate limit) |
| `GET /_zoo/health` | contract health (`build.built_at` set by `-ldflags`) |
| `GET /_zoo/probe` | aggregated probe, see below |
| `POST /_zoo/chain/shop-order` `{"trace"}` | starts the chain for `ZOO-1`, 202, 10 per minute |
| `GET /_zoo/trace/<id>` | hops from the PG `hops` table |

Probe checks, in order: `postgres` (zoo_probe write, read, delete), `redis` (SET with TTL,
GET, DEL), `fulfiller` (heartbeat at most 30 s old), `worker:orders` (via `ORDERS_URL`),
`worker:stock` (via `STOCK_URL`, reads the stock table), `peer:catalog-api` and
`peer:rails-queue` (signed `GET /_zoo/verify`; pass only on 200, the right `name`,
`key_fp` equal to ours, and `public_url` equal to our URL variable). The first three run
inside the orders worker, which holds the PG and Redis clients. `vars`:
`INTERNAL_TOKEN` (signs), `WEBHOOK_SECRET` (signs), `CATALOG_URL` (url, peer catalog-api),
`RAILS_URL` (url, peer rails-queue), `ORDERS_URL`, `STOCK_URL`, `ZOO_PANEL_ORIGIN` (plain).

Chain hops written here: `order-created`, `catalog-ok`, `stock-reserved` (orders),
`fulfilled`, `webhook-sent` (fulfiller). A broken step writes `failed` with the reason.

## Variables

Provided by ox: `PORT`, `HOST`, `OX_ENV`, `OX_RELEASE`, `PUBLIC_HOST`, `DATABASE_URL`,
`REDIS_URL`, `ORDERS_URL`, `STOCK_URL`.

Yours (`.env.example`):

| Key | Kind | Value |
|-----|------|-------|
| `INTERNAL_TOKEN` | secret, shared with catalog-api, sveltekit-ssr, axum-cache | from `shared.env` |
| `WEBHOOK_SECRET` | secret, shared with rails-queue, laravel-jobs | from `shared.env` |
| `CATALOG_URL` | url | `https://catalog-api.s2.zoo.sorv.dev` |
| `RAILS_URL` | url | `https://rails-queue.s1.zoo.sorv.dev` |
| `ZOO_PANEL_ORIGIN` | plain | `https://zoo-control.s1.zoo.sorv.dev` |

## Tests

```
cd gateway && go test ./...                  # vectors, CORS, trace ids, rate limit, verify matching, probe
cd orders && bun test                        # vectors, signed catalog request, trace and sku validation
cd stock && uv run pytest                    # reserve validation; set TEST_DATABASE_URL for the row lock test
cd fulfiller && npm ci && node --test        # vectors, signed webhook body, job validation, hops
```

Recorded run:

```
--- PASS: TestSigningVectors, TestServerLabelAndTrace, TestCORS, TestRateLimit,
          TestVerifyPeer, TestProbeAggregates, TestStartValidates
ok  	mesh-shop/gateway
bun test: 4 pass, 0 fail, 16 expect() calls
pytest: 8 passed, 1 skipped (9 passed with TEST_DATABASE_URL)
node --test: tests 4, pass 4, fail 0
```

Local integration run (brew postgres 18 and redis on random ports, all five processes,
fake catalog-api and rails-queue verifying zoo-sig v1): the probe passed all 7 checks,
the chain wrote all 5 hops, both signed calls verified, a bad trace gave 400, and the
11th chain start in a minute gave 429.

## ox check

```
ox check . (manifest: ox.toml)

  app.start                  exec ./gateway/bin/gateway                           declared
  app.health                 /_zoo/health                                         declared
  static.dir                 web/dist                                             declared
  build.commands[0]          cd gateway && go build -ldflags "-X main.builtAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o bin/gateway . declared
  build.commands[1]          cd stock && uv sync --frozen --no-dev                declared
  build.commands[2]          cd fulfiller && npm ci --omit=dev                    declared
  build.commands[3]          cd web && bun run build.ts                           declared
  build.migrate              cd stock && .venv/bin/python migrate.py              declared
  workers.fulfiller          cd fulfiller && exec node fulfiller.js               declared
  workers.orders             cd orders && exec bun run server.ts                  declared
  workers.stock              cd stock && exec .venv/bin/uvicorn main:app --host 127.0.0.1 --port $PORT declared
  tools.bun                  1.3                                                  declared
  tools.go                   1.27                                                 declared
  tools.node                 24                                                   declared
  tools.python               3.13                                                 declared
  tools.uv                   0.11                                                 declared
  services.postgres          postgres 18 (shared)                                 default
  services.redis             redis 8 (only for this project)                      default

  Provided by ox: PORT, HOST, OX_ENV, OX_PROJECT, OX_RELEASE, OX_DATA_DIR, PUBLIC_URL, PUBLIC_HOST, ORDERS_URL, STOCK_URL, DATABASE_URL, REDIS_URL
  Set on the dashboard before the first deploy: INTERNAL_TOKEN, WEBHOOK_SECRET, CATALOG_URL, RAILS_URL, ZOO_PANEL_ORIGIN

Ready to deploy.
```

Notes: `orders` and `web` have no dependencies (Bun built-ins only), so they have no
lockfile and no install step. The gateway uses only the Go standard library (no go.sum).
