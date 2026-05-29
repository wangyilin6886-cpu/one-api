# PR-A 改动说明:从印尼/Xendit 转向全球/Polar

> 项目转向:面向全球用户,只用信用卡/借记卡,接入 Polar(Merchant of Record)。
> 这一 PR 把现有 payment-service 改造成 provider 无关的骨架 + Polar 适配器占位。
> Polar 实际 API 调用 + webhook 签名验证留到 PR-B。

---

## 一、做了什么

### 删除
- `payment/internal/xendit/` 整个目录(Xendit HTTP 客户端 + QR/VA API)
- `payment/internal/api/middleware/xendit_ip_whitelist.go` + 测试 — Polar 用 HMAC 签名,不需要 IP 白名单
- `payment/internal/api/middleware/xendit_token.go` + 测试 — 用统一 `polar_signature` 中间件替代(PR-B)
- `payment/docs/PR2_test_windows.ps1` — Xendit 时代的端到端测试脚本
- `docs/payment/PR1_test_windows.ps1` — 同上
- `docs/payment/PR2_payment_service.md` — Xendit 设计文档

### 新增
- `payment/internal/provider/provider.go` — `PaymentProvider` 抽象接口 + 归一化事件类型
  - `EventCheckoutCompleted` / `EventSubscriptionRenewed` / 等 8 种事件
  - `CheckoutParams` / `CheckoutResult` / `NormalizedEvent` DTO
- `payment/internal/provider/polar.go` — Polar 适配器骨架
  - `NewPolar(cfg)` 含启动期参数校验
  - `CreateCheckout` / `VerifyWebhook` 当前返回 `ErrNotImplemented`(PR-B 实现)
- `payment/internal/api/middleware/helpers.go` — 共享 `respondError`

### 重写
| 文件 | 改动 |
|------|------|
| `model/order.go` | 删 `amount_idr`/`fee_idr`/`net_idr`/`exchange_rate`/`qr_string`/`va_number`/所有 `xendit_*`/`payment_method`;加 `amount_usd_cents`/`currency`/`provider`/`provider_checkout_id`/`provider_payment_id`/`order_type`/`subscription_id`;状态机不变 |
| `model/refund.go` | `amount_idr` → `amount_usd_cents` + `currency`;`xendit_refund_id` → `provider_refund_id` |
| `model/webhook_event.go` | `xendit_resource_id` → `provider_resource_id`;新增 `provider` 列 |
| `model/payment_config.go` | 删 Xendit token 三件套 / IP 白名单 / 汇率;加 `polar_*` 配置项 + `checkout_*_url` |
| `service/order_service.go` | 用 `provider.PaymentProvider` 接口替换 `*xendit.Client`;`QuotaFromCents` 替换基于汇率的转换;`generateOrderNo` 前缀 `IDR` → `ORD` |
| `service/webhook_service.go` | 输入从 `NormalizedWebhook`(Xendit 字段)改为 `IncomingWebhook` + `provider.NormalizedEvent`;`isTerminalSuccessStatus`(字符串匹配)改为 `isCreditTriggering`(枚举匹配);金额对比从 IDR 换成 USD cents |
| `service/alerter.go` | `XenditError` → `ProviderError`(其他不变) |
| `errors/codes.go` | **加中文翻译**(En/Zh/Id 三语);删 Xendit 专属错误码 `WebhookBadToken`/`IPNotAllowed`/`UnsupportedMethod`;加 `WebhookBadSignature`/`UnsupportedCurrency`/`ProviderCallFailed` |
| `api/handler/order_handler.go` | DTO 从 IDR 改 USD cents;移除 `payment_method` 字段;加 `customer_email` |
| `api/handler/webhook_handler.go` | 三个 URL(`/webhooks/xendit/{qris,va,invoice}`)合并为一个 `/webhooks/polar`;签名验证下沉到 provider 适配器 |
| `api/router.go` | 同上 |
| `config/config.go` | 删 `XENDIT_*`;加 `POLAR_ACCESS_TOKEN`/`POLAR_SANDBOX`/`POLAR_BASE_URL` |
| `cmd/server/main.go` | wiring 从 Xendit 客户端改为 Polar provider |
| `migrations/001_initial.sql` | 全部表结构重写,匹配新模型 |
| `docker-compose.yml` | TZ 从 `Asia/Jakarta` → `UTC`;Xendit 环境变量替换为 Polar |
| `.env.example` | 同上;含 payment_config 运行时配置说明 |
| `payment/README.md` | 全部重写 |
| 所有测试 | 重写以使用 `fakeProvider` 替代 Xendit 客户端;测试场景未减少 |

---

## 二、关键决策

### 1. Provider 抽象的边界
`PaymentProvider` 接口只暴露两个方法:`CreateCheckout` + `VerifyWebhook`。其他都是它内部的事。原因:
- 我们只关心"创建付款"和"消化付款通知"两件事
- 接口窄,适配器实现简单,mock 容易
- 加新 provider(将来若切 Paddle)只改一个文件

### 2. 不再做 IP 白名单
Polar 用 HMAC over body 签名 webhook,签名验证已经够强。Xendit 时代加 IP 白名单是因为它们只签 token(可重放、可猜测),需要纵深防御。Polar 不需要。

### 3. 三语错误码(EN + ZH + ID)
之前是 EN + ID 双语。你定的方向是"中文英文印尼语",所以加了中文。`PickLang` 按 `Accept-Language` 分发:`zh*` → 中文,`id*` → 印尼语,其他 → 英文。

### 4. order_no 前缀 `IDR` → `ORD`
之前的 `IDR20240517XXX...` 字面意思是"印尼盾订单",误导。改成 `ORD`(generic ORDER)。所有现有印尼订单数据反正要清空,无迁移问题。

### 5. 金额单位:USD cents (int64)
- `$5 = 500 cents`,`$19 = 1900 cents`,`$99 = 9900 cents`
- 不用浮点,不用 `decimal.Decimal`,内存里直接 int64
- 历史汇率字段(`exchange_rate`)删除——USD 计价没有汇率概念

### 6. Polar 运行时配置入 DB
跟 Xendit 时代一样:**admin 可编辑的 secret 放 `payment_config` 表**,启动期 secret(只 `POLAR_ACCESS_TOKEN`)放环境变量。原因:
- webhook secret 需要轮换(改 DB 一行 + 重启即可,不动 .env 不重新部署)
- product_id / org_id 经常因为切产品/换 sandbox 而变

---

## 三、测试现状

```
go test ./...
ok  github.com/songquanpeng/one-api/payment/internal/model    
ok  github.com/songquanpeng/one-api/payment/internal/service  coverage: 76.6%
```

21 个测试全部通过:
- 状态机:`TestOrder_StateMachine`、`TestOrder_IsPostPayTerminal`
- 订单创建:`TestQuotaFromCents`、`TestOrderService_CreateOrder_HappyPath`、`AmountBounds`、`UnsupportedCurrency`、`ProviderFails_OrderMarkedFailed`
- order_no 生成:`TestGenerateOrderNo`
- HMAC 签名:`TestTopupClient_*`(3 个,验证和 PR#1 协议逐字节一致)
- Webhook 主流程:`PendingPaidCredited`、`DuplicateDelivery`
- 迟到回调安全(关键):`LateCallback_Expired`、`LateCallback_Canceled`
- 边界:`UnknownOrder`、`NonCreditTriggering`、`AmountMismatch_StillCredits_AndAlerts`、`OneAPIRefusal_LeavesPaidForRetry`
- 幂等:`CreditOrder_IdempotentOnTopupCallback`
- 事件分类:`TestIsCreditTriggering`

---

## 四、PR-A 还**没**做的事(留给后续 PR)

| 项 | PR | 说明 |
|---|---|---|
| Polar `POST /v1/checkouts` 真实 HTTP 调用 | **PR-B** | 现在是 `ErrNotImplemented` |
| Polar webhook HMAC 签名验证(Standard Webhooks 规范) | **PR-B** | 现在是 `ErrNotImplemented` |
| Polar webhook 事件类型映射(`order.created`/`subscription.created` 等 → `NormalizedEvent`) | **PR-B** | 同上 |
| 端到端测试脚本(PowerShell + curl)| **PR-B** | 删了 Xendit 时代两份,新版等 PR-B |
| 订阅:`subscriptions` 表 + 月度 cron + 分组分配 | **PR-C** | 接口已预留 `EventSubscription*` |
| 退款 + 管理员后台 | **PR-D** | `refunds` 表已 migrate,代码未写 |
| 一次性补 `set-group` 端点到 one-api(订阅升级用)| **PR-C** | 接 one-api 第二次小改动 |

---

## 五、上线前必须做的事(PR-B 之后也不变)

1. **Polar 账号**:申请 Polar(https://polar.sh),通过审核。我推荐:
   - 业务描述写"global LLM API gateway with prepaid credits"
   - 把退款政策 / 服务条款 / 隐私政策准备好(Polar 申请时要)
2. **生成 INTERNAL_API_SECRET**(`openssl rand -hex 32`),`.env` 填进去
3. **创建 Polar 产品**:
   - 一个"One-shot Top-up"产品(用于一次性充值),抓 product_id
   - PR-C 之后再加 Pro / Max 订阅产品
4. **注册 webhook URL**:Polar dashboard → Webhooks → 添加 `https://<your-domain>/webhooks/polar`,抓 secret
5. **填 `payment_config` 表**:
   ```sql
   UPDATE payment_config SET config_value='<token>' WHERE config_key='polar_webhook_secret';
   UPDATE payment_config SET config_value='<org id>' WHERE config_key='polar_organization_id';
   UPDATE payment_config SET config_value='<product id>' WHERE config_key='polar_topup_product_id';
   ```
6. **DNS + HTTPS**:Polar 不接受 HTTP webhook URL,生产必须 HTTPS

---

## 六、回滚方案

如果 Polar 申请被拒、或者中途要切其他 MoR(Paddle / Lemon Squeezy):
- 实现一个 `paddle.go` / `lemonsqueezy.go` 满足 `PaymentProvider` 接口
- 改 `cmd/server/main.go` 一行,从 `provider.NewPolar(...)` 改为新的
- 数据库不动(`provider` 列已经预留)
- 已存在的 Polar 订单留在 DB 里,新订单走新 provider
