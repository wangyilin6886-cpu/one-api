# payment-service

Standalone Go service that brokers card payments for one-api users worldwide
via **Polar** (https://polar.sh), a Merchant of Record. Polar handles the
card processing, global VAT/sales tax, invoicing, and refunds; we listen
to their webhooks and credit user quota.

## Pivot history

| Iteration | Target market | Payment provider | Status |
|-----------|---------------|------------------|--------|
| v1 | Indonesia | Xendit (QRIS + BCA VA) | Removed |
| **v2** | **Global** | **Polar (MoR)** | **In progress** |

## PR roadmap

| PR | Scope | Status |
|----|-------|--------|
| PR #1 | one-api `/api/internal/topup` HMAC-authenticated endpoint | Merged into main |
| **PR-A** | Delete Xendit; abstract provider interface; reshape schema; trilingual error codes | **This PR** |
| PR-B | Polar adapter implementation (real `CreateCheckout` + `VerifyWebhook`) + e2e tests | Next |
| PR-C | Subscriptions: subscriptions table, monthly credit cron, group assignment | After PR-B |
| PR-D | Refunds + admin manual-recovery endpoints | After PR-C |

## Architecture

```
        Browser
           │
   1. POST /orders {amount_usd_cents:1000}
           │  (Authorization: <one-api access_token>)
           ▼
   ┌──────────────────────┐         ┌─────────────────┐
   │  payment-service     │ verify  │     one-api     │
   │  (this repo)         ├────────▶│  /api/user/self │
   └─────────┬────────────┘         └─────────────────┘
             │
             │ 2. POST /v1/checkouts (Polar API)
             ▼
   ┌──────────────────────┐
   │       Polar          │
   │  (Merchant of Record)│
   └─────────┬────────────┘
             │  3. checkout URL
             │     ◄─────── browser redirected here
             │
             │  4. user pays via card (Polar handles 3DS, tax, etc.)
             │
             │  5. POST /webhooks/polar (signed)
             ▼
   ┌──────────────────────┐         ┌─────────────────┐
   │  payment-service     │ signed  │    one-api      │
   │  Process()           ├────────▶│ /api/internal   │
   │  • verify signature  │ HMAC    │  /topup         │
   │  • dedupe (5 layers) │         │ credits quota   │
   │  • credit one-api    │         └─────────────────┘
   └──────────────────────┘
```

## Five-layer idempotency

Top-up correctness rests on five layers; lose any one and we double-credit:

1. **Provider signature verification** -> `provider.Polar.VerifyWebhook`
2. *(reserved)* IP whitelisting is no longer needed — Polar's HMAC over the
   body is stronger than CIDR matching.
3. **`UNIQUE(event_type, provider_resource_id)`** on `webhook_events` -> dedupes provider retransmissions
4. **Order state machine + `SELECT FOR UPDATE`** in `webhook_service` -> late callbacks against terminal-non-credited orders escalate to `needs_manual_review`
5. **`UNIQUE(order_no, action_type)`** on `topup_callbacks` + one-api's `UNIQUE(order_no, action)` on `internal_topup_records` -> dedupes outgoing /api/internal/topup calls

## Directory layout

```
payment/
├── cmd/server/main.go               entry point: config → DB → provider → services → HTTP → cron
├── internal/
│   ├── api/                         HTTP router + handlers + middleware
│   │   ├── handler/
│   │   │   ├── order_handler.go     POST /orders, GET /orders/:no
│   │   │   ├── webhook_handler.go   POST /webhooks/polar
│   │   │   └── health_handler.go    GET /healthz
│   │   ├── middleware/
│   │   │   ├── user_auth.go         forwards Authorization to one-api /api/user/self
│   │   │   ├── request_logger.go
│   │   │   └── helpers.go
│   │   └── router.go
│   ├── config/config.go             env-var bootstrap
│   ├── cron/                        background sweepers
│   │   ├── expire_orders.go         marks pending orders expired
│   │   └── topup_retry.go           retries failed credit-to-one-api calls
│   ├── errors/codes.go              en / zh / id trilingual error catalog
│   ├── logger/logger.go             zap wrapper
│   ├── model/                       5 GORM tables + state machine
│   ├── provider/                    payment-provider abstraction
│   │   ├── provider.go              PaymentProvider interface + normalized types
│   │   └── polar.go                 Polar adapter (PR-A: stub; PR-B: real)
│   ├── repository/                  DAO layer
│   └── service/                     business logic
│       ├── order_service.go
│       ├── webhook_service.go
│       ├── topup_client.go          HMAC-signed call to one-api
│       └── alerter.go               log-based; Telegram/email later
├── migrations/
│   ├── 00_create_schema.sql         CREATE DATABASE oneapi_payment (mysql initdb)
│   └── 001_initial.sql              explicit DDL for production review
├── Dockerfile                       alpine + curl for healthchecks
├── go.mod
└── README.md
```

## Quota pricing (PR-A baseline)

Payment-service does a transparent **1:1 USD → quota** conversion:

```
quota = floor(amount_usd_cents × 500_000 / 100)
      = amount_usd_cents × 5_000
```

Examples:

| USD | Quota |
|-----|-------|
| $5  | 2,500,000 |
| $19 (Pro) | 9,500,000 |
| $99 (Max) | 49,500,000 |

**Profit margin is taken on the consumption side, NOT here.** Configure
`group_ratio > 1` in one-api per user group to make each LLM call burn more
quota than the upstream provider charges. This keeps the payment service's
pricing logic clean and lets you A/B without touching this codebase.

## Run locally (docker-compose)

```sh
# 1. fill in secrets
cp .env.example .env
openssl rand -hex 32          # paste into INTERNAL_API_SECRET
# paste your Polar SANDBOX access token into POLAR_ACCESS_TOKEN

# 2. start everything
docker compose up -d

# 3. after first startup, fill in the runtime config (one-time):
docker exec -it mysql mysql -u oneapi -p oneapi_payment
> UPDATE payment_config SET config_value='<polar org id>'         WHERE config_key='polar_organization_id';
> UPDATE payment_config SET config_value='<polar topup product>'  WHERE config_key='polar_topup_product_id';
> UPDATE payment_config SET config_value='<polar webhook secret>' WHERE config_key='polar_webhook_secret';
> exit

# 4. restart payment-service so it picks up the new payment_config
docker compose restart payment-service
```

## Tests

```sh
cd payment
go test ./...
```

21 tests covering: state machine, quota math, HMAC signing, webhook
idempotency, late-callback escalation, retry-safe credit flow. Service-layer
coverage: ~77%.

## Endpoints

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/healthz` | none | DB ping, version |
| POST | `/orders` | one-api token | create top-up |
| GET | `/orders/:order_no` | one-api token | fetch order status |
| POST | `/webhooks/polar` | HMAC (Polar) | inbound provider event |

## Operator checklist before going live

- [ ] `INTERNAL_API_SECRET` is 64 random hex chars, **identical on both services**
- [ ] `POLAR_ACCESS_TOKEN` is the production token (not `polar_oat_sandbox_*`)
- [ ] `POLAR_SANDBOX=false` in production
- [ ] All four Polar values in `payment_config` are filled in (`polar_organization_id`, `polar_topup_product_id`, `polar_webhook_secret`, `checkout_success_url`)
- [ ] `PAYMENT_BASE_URL` matches the public hostname registered with Polar
- [ ] Database backup cron is enabled - the `orders` table is the source of truth for outstanding obligations
- [ ] Polar dashboard's "Restricted countries" list aligned with your sanctions policy (Polar applies defaults, but verify)
