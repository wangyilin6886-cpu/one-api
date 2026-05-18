# PR #1 改动说明:one-api 内部充值端点

> 这是印尼支付模块三段式上线的第一段。这一段把"加额度 / 退额度"
> 的 RPC 入口加到 one-api,后续 payment-service(PR #2)会调用它。
> 本 PR 单独可合并、可上线,不依赖 payment-service。

---

## 一、改了哪些文件

| 文件 | 状态 | 作用 |
|------|------|------|
| `model/internal_topup.go` | **新增** | 新增 `internal_topup_records` 表 + GORM 模型;`UNIQUE(order_no, action)` 复合索引承担 one-api 这一侧的幂等。 |
| `model/main.go` | 修改 | 在 `migrateDB()` 末尾加一行 `AutoMigrate(&InternalTopupRecord{})`。 |
| `common/ctxkey/key.go` | 修改 | 新增 `InternalAuth` ctxkey,中间件验签成功后写到 Gin Context 供下游 handler 区分。 |
| `middleware/internal_auth.go` | **新增** | HMAC-SHA256 验签中间件,读取 `INTERNAL_API_SECRET` 环境变量,60s 防重放窗口。 |
| `middleware/internal_auth_test.go` | **新增** | 12 个单元测试,覆盖 happy path / 缺密钥 / 短密钥 / 缺头 / 时间戳过期 / 时间戳未来 / 签名不匹配 / body 篡改 / path 篡改 / querystring 不参与签名 / 签名格式非法。 |
| `controller/internal_topup.go` | **新增** | `POST /api/internal/topup` 处理器。事务 + `SELECT FOR UPDATE` 锁用户行,支持 `topup` / `refund` 两个 action。 |
| `controller/internal_topup_test.go` | **新增** | 8 个集成测试 + 1 个工具函数测试,SQLite 落盘 DB,覆盖 happy path / 幂等重放 / 退款成功 / 余额不足 / 校验 / 用户不存在 / 同 order_no 不同 action 互不冲突 / 重复键错误识别。 |
| `router/api.go` | 修改 | 在 `SetApiRouter` 顶部新增一个 `/api/internal` group,绑定 `InternalAuth` 中间件,**故意放在 `apiRouter` 之前**,这样不继承全局 IP 限流和 gzip 中间件。 |

为什么比设计稿多出来一个 `model/internal_topup.go`:设计稿里 payment 侧的
`topup_callbacks` 已经做了一层幂等,但如果 payment-service 在"INSERT 幂等行"
和"调 one-api"之间崩溃,重启后 payment-service 必须重试同一个 HTTP 请求,
one-api 这一侧需要靠自己识别重放。在你回答里你确认走"加新表"路线。

---

## 二、HMAC 签名规范(payment-service 必须按字节对齐)

```
canonical = timestamp + "\n" + METHOD + "\n" + path + "\n" + hex(sha256(body))
signature = hex(hmac_sha256(secret, canonical))
```

- `timestamp` : Unix 秒,十进制字符串。**请求头 `X-Internal-Timestamp`**。
- `METHOD`    : 大写 HTTP 方法(`POST`)。
- `path`      : 只用路径,不含 querystring(回答确认过)。例如 `/api/internal/topup`。
- `body`      : 请求体原始字节;GET 请求时是空串的 sha256。
- `signature` : **小写**十六进制,**请求头 `X-Internal-Signature`**。
- 重放窗口:`|now - timestamp| > 60s` 拒绝。
- 共享密钥:环境变量 `INTERNAL_API_SECRET`,至少 32 字符,否则中间件直接 503 拒绝所有请求。

Go 端参考实现(payment-service PR #2 里会用到):

```go
ts := strconv.FormatInt(time.Now().Unix(), 10)
bodyHash := sha256.Sum256(body)
canonical := ts + "\nPOST\n/api/internal/topup\n" + hex.EncodeToString(bodyHash[:])
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(canonical))
sig := hex.EncodeToString(mac.Sum(nil))

req.Header.Set("X-Internal-Timestamp", ts)
req.Header.Set("X-Internal-Signature", sig)
```

---

## 三、API 契约

### 请求 `POST /api/internal/topup`

```json
{
  "action": "topup",
  "order_no": "IDR202405171234567890123456",
  "user_id": 42,
  "quota": 1000000,
  "remark": "QRIS payment via Xendit (test)"
}
```

字段:

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `action` | string | 是 | `"topup"` 或 `"refund"` |
| `order_no` | string ≤64 | 是 | 全链路幂等键 |
| `user_id` | int >0 | 是 | one-api `users.id` |
| `quota` | int64 >0 | 是 | quota 数,**始终为正数**;refund 时由 one-api 自动取反 |
| `remark` | string ≤255 | 否 | 写到 `logs.content` 里 |

### 响应

**首次成功**:

```json
{
  "success": true,
  "message": "",
  "data": {
    "order_no": "IDR202405171234567890123456",
    "action": "topup",
    "user_id": 42,
    "quota": 1000000,
    "new_balance": 1500000,
    "idempotent_replay": false
  }
}
```

`quota` 为带符号值:`topup` 返回正数,`refund` 返回负数,方便 payment-service 直接入账。
`new_balance` 是事务提交后用户的余额。

**幂等重放**(同 `(order_no, action)` 第二次以后到达):
返回**首次**的结果,`idempotent_replay: true`,不再触动用户余额。

**错误**(`success: false`,带 `code`):

| HTTP | code | 含义 |
|------|------|------|
| 400 | `invalid_request_body` | JSON 反序列化失败 |
| 400 | `invalid_parameter` | 字段校验失败(见上表) |
| 401 | `missing_internal_auth_headers` / `invalid_timestamp_format` / `timestamp_out_of_window` / `signature_mismatch` / `invalid_signature_format` | HMAC 中间件拒绝 |
| 404 | `user_not_found` | `user_id` 不存在 |
| 409 | `insufficient_balance` | refund 时用户余额不够,**事务已回滚,无副作用** |
| 500 | `transaction_failed` / `lookup_failed` | 内部错误 |
| 503 | `internal_auth_not_configured` | 服务端 `INTERNAL_API_SECRET` 未配 / 短于 32 字节 |

---

## 四、并发与幂等保证

事务体内的步骤(`controller/internal_topup.go:InternalTopup` 里 `model.DB.Transaction(...)`):

1. **`SELECT FOR UPDATE` 锁住 `users` 行**(沿用 `model.Redeem` 的写法)。
   一旦锁住,同一个用户的并发 topup/refund 排队等待。
2. **计算新余额**;refund 时如果 `user.Quota < quota`,直接返回 `errInsufficientBalance`,
   事务回滚,**幂等行也不会落库**(这是关键:失败后允许同 order_no 重试)。
3. **`INSERT internal_topup_records`**;命中 `UNIQUE(order_no, action)` 则返回
   `errDuplicateRecord`,事务回滚,外层从 DB 读出已存在的记录返回幂等响应。
4. **`UPDATE users SET quota = quota ± N WHERE id = ?`**(用 `gorm.Expr` 保证 DB 端做加减)。
5. 事务提交。

事务**之外**做的事:

- 写 `logs` 表(`RecordTopupLog`):走 `LOG_DB`,即使失败也不影响主流程。
- 刷新 Redis 用户额度缓存(`CacheUpdateUserQuota`):失败只打日志,不影响响应。

**绕开批量写**:`model.IncreaseUserQuota` / `DecreaseUserQuota` 在
`BATCH_UPDATE_ENABLED=true` 时会异步排队写库,这对支付来说不可接受
(用户付钱了但 quota 几秒后才到账)。因此本 handler **直接在事务里
用 `gorm.Expr` 写 `users.quota`**,绕过批量队列。批量队列里其他渠道
对同一用户的 delta 仍然会在批量 flush 时增量应用,**两条路径互不破坏**。

---

## 五、数据库迁移(MySQL 8)

GORM `AutoMigrate` 已经会自动建表。如果想手动跑或预审,这是等价的 DDL:

```sql
CREATE TABLE `internal_topup_records` (
  `id`          BIGINT       NOT NULL AUTO_INCREMENT,
  `order_no`    VARCHAR(64)  NOT NULL,
  `action`      VARCHAR(24)  NOT NULL,
  `user_id`     INT          NOT NULL,
  `quota`       BIGINT       NOT NULL,
  `new_balance` BIGINT       NOT NULL,
  `remark`      VARCHAR(255) DEFAULT '',
  `created_at`  BIGINT       NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_internal_topup_order_action` (`order_no`, `action`),
  KEY `idx_internal_topup_records_user_id` (`user_id`),
  KEY `idx_internal_topup_records_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

回滚(如果需要):

```sql
DROP TABLE IF EXISTS `internal_topup_records`;
```

注:由于 `controller/internal_topup.go` 启动时不会去探测旧表,
直接 DROP 即可,不需要 down migration 脚本。

---

## 六、上线前必须做的事

1. **生成强随机的 `INTERNAL_API_SECRET`**(≥32 字符,推荐 64):
   ```bash
   openssl rand -hex 32
   ```
2. **写入 one-api 的环境变量**(docker-compose 或 systemd):
   ```
   INTERNAL_API_SECRET=<上一步生成的>
   ```
3. **同一个值后续会塞进 payment-service 的环境变量**(PR #2)。两端必须一致。
4. **不要把这个密钥提交进 git**;`.env` 文件加 `.gitignore`,或用 Docker secrets。
5. 启动 one-api,确认日志里**没有** `INTERNAL_API_SECRET is unset or too short` 报错。

---

## 七、本地测试(可直接复制粘贴)

启动 one-api(SQLite 模式,最小可跑):

```bash
export INTERNAL_API_SECRET="$(openssl rand -hex 32)"
export PORT=3000
go run main.go
# 等待日志出现 "server started on http://localhost:3000"
```

另开一个 terminal,准备好辅助变量:

```bash
SECRET="$INTERNAL_API_SECRET"   # 必须和 one-api 进程的环境变量一致
ROOT_TOKEN="$(curl -s http://localhost:3000/api/status | jq -r 'now | tostring')"  # 占位,后面要替换
USER_ID=1   # root 用户;实际可用任何已存在用户
```

**用 root access token 给自己加点起始 quota**(给 refund 测试预留余额):

```bash
# 先用 root 账号(默认 root / 123456)登录获取 cookie,或者用 access_token
# 简化起见,这里假设你已经手动通过后台给 user_id=1 加了 quota,
# 或者用已有的 admin 接口:
# curl -X POST http://localhost:3000/api/topup -H 'Authorization: <root_access_token>' \
#      -d '{"user_id":1,"quota":10000000,"remark":"seed for testing"}'
```

**签名 + 调用 topup**:

```bash
ts=$(date +%s)
body='{"action":"topup","order_no":"IDR20240517TEST0001","user_id":1,"quota":1000000,"remark":"test"}'
body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -hex | awk '{print $2}')
canonical=$(printf '%s\nPOST\n/api/internal/topup\n%s' "$ts" "$body_hash")
sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')

curl -sS -X POST http://localhost:3000/api/internal/topup \
  -H "Content-Type: application/json" \
  -H "X-Internal-Timestamp: $ts" \
  -H "X-Internal-Signature: $sig" \
  -d "$body" | jq .
```

期望:`success: true`,`data.new_balance` 等于之前余额 + 1,000,000,`idempotent_replay: false`。

**幂等测试**:**原封不动**再跑一遍同样的命令(同一 `ts` 和 `sig`)。

期望:`success: true`,`idempotent_replay: true`,`data.new_balance` 不变(没有重复加额度)。
另用 SQL 查 `SELECT quota FROM users WHERE id=1;` 余额没翻倍。

> 注意:60s 之后那个签名会失效(防重放窗口)。要重测幂等就用新的 ts/sig
> 但**保持 order_no 不变**。

**签名失败测试**(故意改坏签名):

```bash
curl -sS -X POST http://localhost:3000/api/internal/topup \
  -H "Content-Type: application/json" \
  -H "X-Internal-Timestamp: $ts" \
  -H "X-Internal-Signature: deadbeef" \
  -d "$body"
# 期望:401, code=signature_mismatch (或 invalid_signature_format)
```

**退款测试**(refund):

```bash
ts=$(date +%s)
body='{"action":"refund","order_no":"IDR20240517TEST0001","user_id":1,"quota":500000,"remark":"partial refund"}'
body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -hex | awk '{print $2}')
canonical=$(printf '%s\nPOST\n/api/internal/refund\n%s' "$ts" "$body_hash")  # 故意把 path 写错试试
sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')

curl -sS -X POST http://localhost:3000/api/internal/topup \
  -H "Content-Type: application/json" \
  -H "X-Internal-Timestamp: $ts" \
  -H "X-Internal-Signature: $sig" \
  -d "$body"
# 期望:401, code=signature_mismatch (path 不对)
```

修正 path 重签:

```bash
ts=$(date +%s)
body='{"action":"refund","order_no":"IDR20240517TEST0001","user_id":1,"quota":500000,"remark":"partial refund"}'
body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -hex | awk '{print $2}')
canonical=$(printf '%s\nPOST\n/api/internal/topup\n%s' "$ts" "$body_hash")
sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')

curl -sS -X POST http://localhost:3000/api/internal/topup \
  -H "Content-Type: application/json" \
  -H "X-Internal-Timestamp: $ts" \
  -H "X-Internal-Signature: $sig" \
  -d "$body" | jq .
# 期望:success: true, data.quota = -500000, data.new_balance 比上一步少 500000
```

**余额不足退款测试**:用一个新的 order_no 和一个余额很小的用户:

```bash
# 假设 user_id=2 余额只有 100
ts=$(date +%s)
body='{"action":"refund","order_no":"IDR20240517INSUF","user_id":2,"quota":99999999,"remark":"should fail"}'
body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -hex | awk '{print $2}')
canonical=$(printf '%s\nPOST\n/api/internal/topup\n%s' "$ts" "$body_hash")
sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')

curl -i -sS -X POST http://localhost:3000/api/internal/topup \
  -H "Content-Type: application/json" \
  -H "X-Internal-Timestamp: $ts" \
  -H "X-Internal-Signature: $sig" \
  -d "$body"
# 期望:HTTP/1.1 409, code=insufficient_balance
# 然后 SELECT * FROM internal_topup_records WHERE order_no='IDR20240517INSUF';
# 应该返回 0 行(事务回滚了,记录不会留下,允许后续重试)
```

---

## 八、单元测试一栏

```bash
# 全部跑过
go test ./middleware/ ./controller/ -run "TestInternalAuth|TestInternalTopup|TestIsDuplicateKeyError" -v
```

| 测试 | 覆盖点 |
|------|--------|
| `TestInternalAuth_HappyPath` | 正确签名通过 |
| `TestInternalAuth_BodyAvailableAfterMiddleware` | 中间件读 body 后,下游 handler 仍能读到完整 body |
| `TestInternalAuth_MissingSecret` | 服务端没配 `INTERNAL_API_SECRET` → 503 |
| `TestInternalAuth_ShortSecret` | 密钥 < 32 字节 → 503 |
| `TestInternalAuth_MissingHeaders` | 缺 ts/sig 头 → 401 |
| `TestInternalAuth_TimestampReplay` | ts 在过去 120s → 401 防重放 |
| `TestInternalAuth_FutureTimestamp` | ts 在未来 120s → 401 |
| `TestInternalAuth_SignatureMismatch` | secret 不对 → 401 |
| `TestInternalAuth_TamperedBody` | 签名后篡改 body → 401 |
| `TestInternalAuth_TamperedPath` | 签名 path 与请求 path 不同 → 401 |
| `TestInternalAuth_QueryStringNotSigned` | 加 querystring 不影响签名验证(故意不签 query) |
| `TestInternalAuth_InvalidSignatureHex` | sig 不是 hex → 401 |
| `TestInternalTopup_HappyPath` | 第一次 topup 成功,余额正确 |
| `TestInternalTopup_Idempotency` | 同 order_no 重放,余额不翻倍 |
| `TestInternalTopup_Refund_HappyPath` | refund 扣 quota 成功 |
| `TestInternalTopup_Refund_InsufficientBalance` | refund 余额不足 → 409,余额不变,无幂等行残留(允许重试) |
| `TestInternalTopup_Validation` | 8 个参数校验子用例 |
| `TestInternalTopup_UserNotFound` | user_id 不存在 → 404 |
| `TestInternalTopup_DifferentActionsSameOrderNo` | 同 order_no + 不同 action 互不冲突,先 topup 后 refund 都能跑 |
| `TestIsDuplicateKeyError` | 三个 DB 驱动各自的重复键错误识别 |

---

## 九、已知限制 / 设计决策

1. **SELECT FOR UPDATE 不在 SQLite 上生效**:`tx.Set("gorm:query_option", "FOR UPDATE")`
   在 SQLite 上会被忽略(语法不支持)。这与 `model.Redeem` 的现有写法一致。
   生产用 MySQL 8,不会受影响。

2. **`Log.Quota` 字段是 `int`**(不是 int64):你确认保持现状。
   印尼市场单笔上限 10,000,000 IDR 对应约 3 亿 quota,远低于 int32 上限 21 亿,**不会溢出**。
   如果将来产品扩大到大客户 / 法币贬值,需要重新评估。

3. **失败请求不入幂等表**:`insufficient_balance` 等业务错误回滚整个事务,
   `internal_topup_records` 不会留行。这是有意的:允许 payment-service
   先充值后重试。如果未来想"失败也幂等"(避免重试风暴),要单独再设计。

4. **`internal_topup_records` 一直增长**:目前不清理。一年内单租户在
   印尼市场最多几万行,完全可以接受。后续要清理时按 `created_at` 删
   90 天前的即可。

5. **没有把响应做 i18n**:错误 `message` 是英文,因为消费方是另一个后端服务
   (payment-service),不是终端用户。Payment-service 自己负责把 `code` 翻译成
   印尼语 / 英文给前端展示。

6. **路径 `/api/internal/topup`** 不能改:HMAC 签名把 path 也算进去了,改路径
   就要两端同步改。如果未来想加 `/api/internal/refund` 单独路径,反而更直观,
   但需要小心 path 字段同步。
