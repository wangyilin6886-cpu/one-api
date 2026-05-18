# payment-service

Standalone Go service that creates Xendit top-up orders for one-api users and
credits one-api on payment confirmation. Targets the Indonesian market;
currency is IDR, payment methods are QRIS and BCA Virtual Account.

This is **PR #2** of three:

| PR | Scope |
|----|-------|
| #1 | one-api: `POST /api/internal/topup` HMAC-authenticated endpoint |
| **#2** | **payment-service: order creation, Xendit, webhooks, cron** |
| #3 | refund flow + admin manual-recovery endpoints |

## Architecture decision

- **Independent DB schema** (`oneapi_payment`), **independent process**, port 3001.
- one-api and payment-service share `INTERNAL_API_SECRET` (HMAC-SHA256) for the
  service-to-service `POST /api/internal/topup` call.
- Webhook ingestion uses Xendit's per-URL `x-callback-token`, stored in
  `payment_config` rows so admins can rotate tokens via SQL without a restart.

## Five-layer idempotency

Top-up correctness rests on five layers; lose any one and we double-credit:

1. **IP whitelist** (`middleware/xendit_ip_whitelist.go`) - rejects non-Xendit IPs.
2. **Shared-token check** (`middleware/xendit_token.go`) - `x-callback-token` from `payment_config`.
3. **`UNIQUE(event_type, xendit_resource_id)`** on `webhook_events` - dedupes Xendit retransmissions.
4. **Order state machine + `SELECT FOR UPDATE`** (`service/webhook_service.go`) -
   late callbacks against terminal-non-credited orders escalate to `needs_manual_review` (corrections 1, 2).
5. **`UNIQUE(order_no, action_type)`** on `topup_callbacks` + one-api's
   `UNIQUE(order_no, action)` on `internal_topup_records` (PR #1) -
   dedupes outgoing calls to one-api.

## Directory layout

```
payment/
├── cmd/server/main.go              # entry point
├── internal/
│   ├── api/                        # HTTP router + handlers + middleware
│   │   ├── handler/
│   │   ├── middleware/
│   │   └── router.go
│   ├── config/                     # env-var bootstrap
│   ├── cron/                       # background sweepers
│   │   ├── expire_orders.go
│   │   └── topup_retry.go
│   ├── errors/                     # bilingual error catalog
│   ├── logger/                     # zap wrapper
│   ├── model/                      # 5 GORM tables + state machine
│   ├── repository/                 # DAO
│   ├── service/                    # business logic
│   │   ├── order_service.go
│   │   ├── webhook_service.go
│   │   ├── topup_client.go         # HMAC-signed call to one-api
│   │   └── alerter.go              # log-based; Telegram/email later
│   └── xendit/                     # thin HTTP wrapper (no SDK)
├── migrations/
│   ├── 00_create_schema.sql        # CREATE DATABASE oneapi_payment (mysql init)
│   └── 001_initial.sql             # explicit DDL for production review
├── docs/
│   └── PR2_test_windows.ps1        # Windows PowerShell e2e test
├── Dockerfile
├── go.mod
└── README.md
```

## Run locally (docker-compose)

```sh
# 1. fill in secrets
cp .env.example .env
openssl rand -hex 32           # paste into INTERNAL_API_SECRET
# paste your Xendit test key into XENDIT_SECRET_KEY

# 2. start everything
docker compose up -d

# 3. seed an order (use a valid one-api user access_token)
curl -X POST http://localhost:3001/orders \
  -H "Authorization: $ONE_API_USER_ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"amount_idr":50000,"payment_method":"qris"}'
```

## Tests

```sh
cd payment
go test ./...
```

32 tests covering: state machine, exchange-rate lockdown, quota flooring,
HMAC signing, IP whitelist (incl. XFF trust modes), token verification,
webhook idempotency, late-callback escalation, retry-safe credit flow.

Service-layer coverage: 77%.

## Endpoints

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/healthz` | none | DB ping, version |
| POST | `/orders` | user (forwards to one-api `/api/user/self`) | create top-up |
| GET | `/orders/:order_no` | user | fetch order status |
| POST | `/webhooks/xendit/qris` | IP + `x-callback-token` | Xendit QRIS event |
| POST | `/webhooks/xendit/va` | IP + `x-callback-token` | Xendit VA event |
| POST | `/webhooks/xendit/invoice` | IP + `x-callback-token` | Xendit Invoice event |

PR #3 will add `/admin/*` endpoints for manual recovery.

## Operator checklist before go-live

- [ ] `INTERNAL_API_SECRET` is 64 random hex chars, **identical on both services**
- [ ] `XENDIT_SECRET_KEY` is the production key (not `xnd_development_*`)
- [ ] `xendit_ip_whitelist_json` row in `payment_config` is set to Xendit's documented IPs
      (NOT the default `0.0.0.0/0`)
- [ ] All three `xendit_webhook_token_*` rows in `payment_config` are filled in
      with the tokens Xendit assigned to each webhook URL
- [ ] `PAYMENT_BASE_URL` matches the public hostname the operator gave Xendit
- [ ] Database backup cron is enabled - the `orders` table is the source of truth
      for outstanding obligations
