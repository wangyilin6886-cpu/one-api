# PR #2 改动说明:payment-service 主体

> 印尼支付模块三段式的第二段。新增 payment-service 独立服务,
> 负责接收下单请求、对接 Xendit(QRIS / BCA VA)、消化 webhook、
> 通过 PR #1 的内部端点给 one-api 用户加额度。
>
> 退款流程留到 PR #3。本 PR 只做 topup。

---

## 一、新增/修改文件清单

```
payment/                                            # 全部新增
├── go.mod, go.sum, Dockerfile, README.md
├── cmd/server/main.go                              # 入口:配置→DB→服务→HTTP→cron
├── internal/
│   ├── api/
│   │   ├── handler/
│   │   │   ├── order_handler.go                    # POST /orders, GET /orders/:no
│   │   │   ├── webhook_handler.go                  # 三个 webhook URL,解析 payload
│   │   │   └── health_handler.go                   # /healthz
│   │   ├── middleware/
│   │   │   ├── xendit_ip_whitelist.go              # 修正 7
│   │   │   ├── xendit_token.go                     # 修正 9
│   │   │   ├── user_auth.go                        # 反向调 one-api /api/user/self
│   │   │   └── request_logger.go
│   │   └── router.go
│   ├── config/config.go                            # 环境变量启动配置
│   ├── cron/
│   │   ├── expire_orders.go                        # 每分钟扫过期订单
│   │   └── topup_retry.go                          # 重试失败的 credit 调用
│   ├── errors/codes.go                             # 双语错误码
│   ├── logger/logger.go                            # zap 包装
│   ├── model/
│   │   ├── order.go                                # 含状态机
│   │   ├── webhook_event.go
│   │   ├── topup_callback.go
│   │   ├── refund.go                               # PR #3 才用,本版只迁移表
│   │   └── payment_config.go                       # 含 seed 函数
│   ├── repository/
│   │   ├── order_repo.go                           # 含 SELECT FOR UPDATE
│   │   ├── webhook_repo.go
│   │   ├── topup_callback_repo.go
│   │   └── payment_config_repo.go                  # 5 分钟内存缓存
│   ├── service/
│   │   ├── order_service.go                        # 订单创建,锁汇率,Xendit 集成
│   │   ├── webhook_service.go                      # 五层幂等核心
│   │   ├── topup_client.go                         # HMAC 签名调 one-api
│   │   ├── alerter.go                              # 告警占位(log 级)
│   │   └── json_helper.go
│   └── xendit/
│       ├── client.go                               # 自研 HTTP 客户端,无 SDK
│       ├── qr.go                                   # POST /qr_codes
│       └── va.go                                   # POST /callback_virtual_accounts
├── migrations/
│   ├── 00_create_schema.sql                        # mysql initdb 自动跑
│   └── 001_initial.sql                             # 手动审计用 DDL
└── docs/
    └── PR2_test_windows.ps1                        # Windows 端到端测试脚本

docker-compose.yml                                  # 修改:加 payment-service,共享网络
.env.example                                        # 修改:加 payment 相关环境变量
docs/payment/PR2_payment_service.md                 # 新增,即本文
```

修改的现有文件:
- `docker-compose.yml`:新增 `payment-service` 服务,新增 `internal_net` 网络。
  调整 one-api 注入 `INTERNAL_API_SECRET` 环境变量(给 PR #1 用)。
- `.env.example`:新增 INTERNAL_API_SECRET、XENDIT_SECRET_KEY、PAYMENT_BASE_URL 等条目。

---

## 二、关键设计决策

### 1. 为什么不用 xendit-go v6 SDK
你确认过手写 HTTP 客户端。`internal/xendit/client.go` 总共 ~150 行。
SDK 的优点(自动 API 同步)不抵其缺点(自动生成的代码层级深、mock 难)。
我们只用 Xendit 的 2 个端点(创建 QR、创建 VA),不需要 SDK 的全套类型。

### 2. /orders 鉴权 = 转发 one-api /api/user/self
你确认过这个方案。优点:one-api 一行代码不用动,payment-service 不需要
自己维护用户表。代价是每次 /orders 都多一次 HTTP 调用(1 RTT 内,
取消支付流量不高,可接受)。

### 3. 五层幂等防御(对应规范修正 5、6)
```
┌─────────────────────────────────────────────────────────────┐
│ Layer 1: IP whitelist     -> middleware/xendit_ip_whitelist │
│ Layer 2: x-callback-token -> middleware/xendit_token        │
│ Layer 3: UNIQUE(event_type,xendit_resource_id) on           │
│          webhook_events                                     │
│ Layer 4: SELECT FOR UPDATE + IsPostPayTerminal() 检查 ->    │
│          service/webhook_service.go                         │
│ Layer 5: UNIQUE(order_no,action_type) on topup_callbacks    │
│          + one-api UNIQUE(order_no,action) (PR #1)          │
└─────────────────────────────────────────────────────────────┘
```
丢任意一层都可能导致重复加额度。代码里 `webhook_service.go` 头部注释
和 `model/order.go::IsPostPayTerminal` 注释把这套规则讲清楚了。

### 4. 修正 1/2 的实现要点 -- 别 rollback!
最初版本里我把"late callback escalation"用 sentinel error 从 GORM
事务里返回出来,结果发现 GORM 把整个事务都回滚了 —— metadata 和
needs_manual_review 都没落库。

修复方式:用枚举值(`outcomeEscalated`)在事务外通信,事务内 return nil
让变更提交。这一坑也有单元测试(`TestWebhookService_LateCallback_Expired`)
卡住了我,所以后续不会再踩。

### 5. 汇率锁定(修正 3)
在 `OrderService.CreateOrder` 里:
```go
amountDec := decimal.NewFromInt(p.AmountIDR)
quotaDec := amountDec.Mul(decimal.NewFromInt(QuotaPerUnit)).Div(rate)
quota := quotaDec.Floor().IntPart()
```
用 `shopspring/decimal` 避免浮点。Floor 保证用户少拿不多拿。
计算完写到 `orders.quota_to_credit`,后续永不重新计算。
单元测试 `TestOrderService_ExchangeRateLockdown` 验证了这一点:改完
payment_config 后旧订单的 quota 不变,新订单用新汇率。

### 6. 数据库时区/datetime 精度
原本所有时间字段都用 `gorm:"type:datetime(3)"`,但 mattn/go-sqlite3
的自动 time.Time 解析只认 `datetime`(不带精度),导致 SQLite 测试
全部 SCAN 失败。

修复:模型 tag 改成 `type:datetime`,生产 MySQL 通过 `migrations/001_initial.sql`
显式使用 `DATETIME(3)`(毫秒精度)。AutoMigrate 路径只用于本地 dev / tests。
生产部署走手动 SQL 迁移。

---

## 三、9 项规范修正在 PR #2 的对应位置

| 修正 | 位置 |
|------|------|
| 1. 过期订单的迟到回调 | `webhook_service.go::processInsertedEvent` 中的 `IsPostPayTerminal()` 分支 |
| 2. 用户取消后到账 | 同上(canceled 也是 post-pay terminal) |
| 3. 汇率锁定 | `order_service.go::CreateOrder` |
| 4. 退款顺序 | PR #3 |
| 5. webhook_events 复合幂等键 | `model/webhook_event.go::WebhookEvent` 的 `uniqueIndex` tag |
| 6. topup_callbacks 复合幂等键 | `model/topup_callback.go::TopupCallback` 的 `uniqueIndex` tag |
| 7. Xendit IP 白名单 + 5min 缓存 + XFF 处理 | `middleware/xendit_ip_whitelist.go` |
| 8. refund_type 三种 | `model/refund.go::RefundType`(表已建,代码 PR #3) |
| 9. webhook token 入库 | `middleware/xendit_token.go` + `model/payment_config.go::SeedRows` |

---

## 四、单元测试

```
$ cd payment && go test ./... -count=1 -v
...
PASS: 32 个
Coverage:
  - service/    77.1%  (核心业务逻辑)
  - model/      44.4%
  - middleware/ 47.2%
```

服务层覆盖率 77%,超过 70% 门槛。其他包覆盖率偏低是因为 thin wrappers
没单独测,但 service 层间接覆盖了 repository / config / xendit client 的
主要路径。

测试列表(选关键的):
- `TestOrder_StateMachine` -- 状态机所有合法/非法转换
- `TestOrder_IsPostPayTerminal` -- 修正 1/2 的关键判定
- `TestOrderService_CreateOrder_QRIS_HappyPath` -- 端到端创建 QR 订单
- `TestOrderService_CreateOrder_VABCA_HappyPath` -- 端到端创建 VA 订单
- `TestOrderService_CreateOrder_AmountBounds` -- 金额上下界拒绝
- `TestOrderService_ExchangeRateLockdown` -- 修正 3 核心
- `TestOrderService_QuotaFloor` -- 不溢出验证
- `TestGenerateOrderNo` -- 1000 次生成无重复,长度 27
- `TestTopupClient_HMACSigning` -- 关键:确保和 PR #1 验签格式逐字节一致
- `TestWebhookService_PendingPaidCredited` -- 主流程 pending → paid → credited
- `TestWebhookService_DuplicateDelivery` -- 同 webhook id 二次投递 = 跳过
- `TestWebhookService_LateCallback_Expired` -- 修正 1
- `TestWebhookService_LateCallback_Canceled` -- 修正 2
- `TestWebhookService_AlreadyCredited` -- 重复 webhook 对已 credited 订单
- `TestWebhookService_UnknownOrder` -- order_no 不存在
- `TestWebhookService_NonTerminalStatus` -- PENDING 状态忽略
- `TestWebhookService_OneAPIRefusal_LeavesPaidForRetry` -- one-api 短时不可用,订单留在 paid 等 cron 重试
- `TestWebhookService_CreditOrder_IdempotentOnTopupCallback` -- 重复调 CreditOrder 不会双花
- `TestIsTerminalSuccessStatus` -- Xendit 状态字符串识别
- `TestIPWhitelist_*` -- 6 个 IP 白名单子用例,涵盖 XFF trust 模式
- `TestTokenAuth_*` -- 4 个 token 验证用例

---

## 五、数据库迁移(MySQL 8)

生产推荐走显式 SQL:

```bash
docker exec -i mysql mysql -uroot -p'OneAPI@justsong' < payment/migrations/00_create_schema.sql
docker exec -i mysql mysql -uroot -p'OneAPI@justsong' oneapi_payment < payment/migrations/001_initial.sql
```

dev / test 可以直接靠 GORM AutoMigrate(`main.go` 启动时自动执行)。

`payment_config` 表的 seed 行由 `PaymentConfigRepo.SeedIfMissing` 在启动时
INSERT(若不存在),不会覆盖已有值。Webhook token 三个字段 seed 时是空串,
**必须人工填**才会让对应 webhook URL 工作。

回滚:

```sql
DROP DATABASE oneapi_payment;
```

---

## 六、上线前必须做的事

1. **生成强随机 INTERNAL_API_SECRET**(和 PR #1 共享):
   ```bash
   openssl rand -hex 32
   ```
   填到 `.env` 的 `INTERNAL_API_SECRET=`。one-api 和 payment-service
   都会读这一行,务必一致。

2. **填 Xendit 测试密钥**:
   Xendit dashboard → Settings → Developers → API Keys → Secret Key(test)
   → 复制到 `.env` 的 `XENDIT_SECRET_KEY=xnd_development_xxx`。

3. **填 Xendit webhook 三件套**(必须在 Xendit dashboard 注册 webhook URL 后再填):
   Xendit dashboard → Settings → Webhooks → Add Callback URL → 复制 Token
   ```sql
   USE oneapi_payment;
   UPDATE payment_config SET config_value='<xendit-给的 token>'
   WHERE config_key='xendit_webhook_token_qris';
   UPDATE payment_config SET config_value='<va 的 token>'
   WHERE config_key='xendit_webhook_token_va';
   UPDATE payment_config SET config_value='<invoice 的 token>'
   WHERE config_key='xendit_webhook_token_invoice';
   ```

4. **锁定 IP 白名单**(默认 `0.0.0.0/0` 是 dev 用的):
   Xendit doc:Xendit 的回调源 IP 列表(参考 Xendit dashboard documentation)
   ```sql
   UPDATE payment_config SET config_value='["<Xendit IP 1>/32","<Xendit IP 2>/32"]'
   WHERE config_key='xendit_ip_whitelist_json';
   ```
   修改后 5 分钟生效,或 `docker compose restart payment-service` 立即生效。

5. **填生产汇率**:默认 `16500`,实际汇率每天可能浮动 ±200,根据财务策略调整:
   ```sql
   UPDATE payment_config SET config_value='17000'
   WHERE config_key='exchange_rate_idr_per_usd';
   ```
   ⚠️ 该改动只影响**新订单**,已存在订单的 quota_to_credit 已锁定。

6. **加 DB 备份 cron**:`oneapi_payment.orders` 是你欠用户的钱。
   `mysqldump` 每天到 S3,至少 30 天版本。

---

## 七、本地端到端测试(Windows PowerShell)

```powershell
# 一次性准备
Copy-Item .env.example .env -Force
# 编辑 .env,填好 INTERNAL_API_SECRET 和 XENDIT_SECRET_KEY

# 启动整套
docker compose up -d
# 等到 docker ps 看到 one-api / payment-service / mysql 都 healthy

# 把 webhook token 填进 payment_config(测试用值)
docker exec mysql mysql -uoneapi -p123456 oneapi_payment -e `
  "UPDATE payment_config SET config_value='test-qris-token' WHERE config_key='xendit_webhook_token_qris';"

# 拿到 root access token(从 one-api 后台或直接查 DB)
$rootToken = docker exec mysql mysql -uoneapi -p123456 'one-api' -N -B `
  -e "SELECT access_token FROM users WHERE username='root';"

# 跑全套
.\payment\docs\PR2_test_windows.ps1 `
  -RootAccessToken $rootToken `
  -WebhookTokenQRIS 'test-qris-token'
```

预期:11 个场景全部 PASS(除场景 10 和 11 的 "需要重启 payment-service" 那一步,
脚本会提示但不强制)。

详细的人工补测项在脚本末尾的 NOTES 块,涵盖:
- 真 Xendit + ngrok 联调
- IP 白名单确定性测试(需要重启)
- 过期 cron(把 order_expiry_minutes 改 1 等 2 分钟)
- topup_retry cron(暂停 one-api 重启验证)
- 用户鉴权失败(伪 Authorization → 401)

---

## 八、已知限制 / 设计决策

1. **payment_config 的内存缓存 TTL 是 5 分钟**。Webhook token、IP 白名单
   等改完后,默认要等 5 分钟才生效。要立即生效就 `docker compose restart
   payment-service`。未来可加 `POST /admin/config/invalidate-cache` 端点。

2. **/orders 没有请求级幂等**。同一用户双击"支付"会创建两个订单,各自
   有不同 order_no。Xendit 也会给两个独立 QR / VA。这是有意的:用户最多
   付一次,另一个会过期。要避免可加 `Idempotency-Key` 头处理。

3. **cron 期间 SELECT FOR UPDATE 可能和 webhook 并发冲突**。冲突时
   webhook 的事务会等 cron 释放锁,然后看到 status=expired,走 late
   callback 路径(`needs_manual_review`),这正是修正 1 的预期行为。

4. **告警靠 logger.Warn 的 "ALERT:" 前缀**。生产建议:
   - 日志聚合上加规则,匹配 `"alert":"needs_manual_review"` 推到 Telegram
   - 后续 PR 直接接 Telegram Bot

5. **VA 只支持 BCA**。`xendit/va.go` 函数已经接受 bank_code 参数,
   handler 层只暴露 `va_bca` 这一个 method 值。要加 BNI / Mandiri 改
   `order_handler.go` 的 method 校验 + 加一个新 method 常量即可。

6. **Xendit webhook 字段可能因产品而变**。我按 Xendit 公开文档的常见
   字段(qr.payment 用 `reference_id`,VA payment 用 `external_id`)
   写的 handler。第一次真实联调时可能要 tweak handler 的 JSON 字段名。
   排查时:看 `webhook_events.raw_payload` 列,里面是 Xendit 原样推过来
   的 body。

7. **未集成 GoPay / OVO / DANA / CC**。代码结构(`PaymentMethod` 枚举
   + `MethodXxx` 常量 + `attachXxx` 函数)为扩展预留。新增一个方法
   大约要改 ≤80 行代码。

8. **`internal_topup_records` 表(PR #1)和 `topup_callbacks` 表(本 PR)
   是冗余的两层防御**。同一个 (order_no, action) 在两侧都存了 UNIQUE
   索引。这是有意的:任何一侧的 bug / 重启状态丢失,另一侧能兜底。
