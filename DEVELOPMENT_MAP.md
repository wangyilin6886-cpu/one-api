# One-API 开发者导览（DEVELOPMENT_MAP）

> 本文档面向二次开发者，作为后续每次找 AI 改代码时随附的项目背景。
> 项目基础：`songquanpeng/one-api`（MIT 协议),Go 后端 + Vue 前端。
> 说明：文档中的"大致行号"基于本仓库当前 commit,后续代码改动后行号会漂移,
> 请以函数名为准。**找不到的事项一律写明"未在代码中找到",未做编造。**

---

## 一、整体目录结构

根目录:

| 目录 | 作用(一句话) |
|------|-------------|
| `main.go` | 程序入口,初始化日志/DB/Redis/缓存,注册路由,启动 HTTP 服务。 |
| `bin/` | 编译输出目录,存放可执行文件。 |
| `common/` | 通用基础库:配置、日志、HTTP 客户端、加解密、工具函数等(不含业务)。 |
| `controller/` | HTTP 控制器层(非中继):用户、Token、渠道、计费、登录、option 等管理类接口。 |
| `docs/` | 项目文档与图片资源。 |
| `middleware/` | Gin 中间件:鉴权、限流、CORS、分发(选渠道)、panic 恢复、缓存控制等。 |
| `model/` | 数据模型与数据库访问层(GORM):user、token、channel、ability、log、redemption、option、cache。 |
| `monitor/` | 渠道健康监控:统计成功率,触发自动禁用。 |
| `relay/` | **核心中继层**:适配器、计费、模型映射、上下游协议转换。 |
| `router/` | 路由注册:`api.go`(管理后台 API)、`relay.go`(/v1 中继)、`web.go`(前端)。 |
| `web/` | 前端 Vue 项目(自带 default / berry / air 多套主题)。 |

`common/` 子目录:

- `blacklist/` 用户黑名单
- `client/` 全局 `http.Client` 配置
- `config/` 配置读取(从环境变量和 Option 表)
- `conv/` 类型转换工具
- `ctxkey/` Gin Context Key 常量集中处
- `env/` 环境变量辅助函数(`Bool/Int/Float64/String`)
- `helper/` 通用工具函数(时间、字符串、JSON 等)
- `i18n/` 国际化
- `image/` 图片下载、尺寸识别、base64 转换
- `logger/` 日志封装
- `message/` 邮件 / 飞书 / 微信推送
- `network/` IP 与子网工具
- `random/` 随机串/数生成
- `render/` SSE 与 JSON 响应渲染辅助
- `utils/` 杂项工具

`relay/` 子目录(中继层核心):

- `adaptor/` **上游适配器**,每家厂商一个子目录(openai/anthropic/gemini/baidu/ali/zhipu/...),共 40 多个。
- `apitype/` API 类型枚举(OpenAI / Anthropic / Gemini / ...)
- `billing/` 计费实现 + `ratio/` 各类倍率(模型倍率、分组倍率、补全倍率)
- `channeltype/` 渠道类型常量与默认 BaseURL 映射
- `constant/` 常量
- `controller/` Relay 业务控制(`text.go` / `image.go` / `audio.go` / `proxy.go` / `helper.go`)
- `meta/` 单次请求的元数据结构 `Meta`
- `model/` 中继层数据结构(请求体、响应体、Usage)
- `relaymode/` 中继模式枚举(ChatCompletions / Completions / Embeddings / Image / Audio / ...)

---

## 二、关键调用链(POST /v1/chat/completions)

下面按"发生顺序"列出文件、函数与作用。**SSE 流式部分单独标注 [流式]**。

### 1. 路由注册

- `router/relay.go:20-26 → SetRelayRouter()`:
  挂载 `/v1` 路由组,串入中间件 `RelayPanicRecover → TokenAuth → Distribute`,
  POST `/v1/chat/completions` 绑定到 `controller.Relay`。

```go
relayV1Router := router.Group("/v1")
relayV1Router.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
{
    relayV1Router.POST("/chat/completions", controller.Relay)
    // ...
}
```

### 2. 鉴权

- `middleware/auth.go:91-151 → TokenAuth()`:
  从 `Authorization: Bearer sk-xxx` 解析 Token,校验 Token 与所属用户状态、过期时间、额度、IP/子网白名单。
  支持 `sk-xxx-channelid` 语法强制走指定渠道(写入 ctxkey `SpecificChannelId`)。
  通过后写入 ctx:`Id`(userId)、`TokenId`、`TokenName`、`Group`、`RequestModel`。

### 3. 限流(全局)

- `middleware/rate-limit.go:93-99 → GlobalAPIRateLimit()`:
  基于 Redis(优先) 或内存的滑动窗口,按 IP 限制 QPS。
  受 `GLOBAL_API_RATE_LIMIT` / `GLOBAL_API_RATE_LIMIT_DURATION` 控制。

### 4. 选渠道(分发)

- `middleware/distributor.go:20-62 → Distribute() → SetupContextForSelectedChannel()`:
  根据用户分组 + 请求模型,从 Ability 表/缓存中挑选可用渠道;
  若 ctx 里有 `SpecificChannelId` 则强制使用该渠道;
  把选中渠道的 ApiKey / BaseURL / ModelMapping / Config / SystemPrompt 写入 ctx 供后续使用。

### 5. 中继入口与失败重试

- `controller/relay.go:45-103 → Relay() → relayHelper()`:
  根据路径推导 `relayMode`,调用 `relay/controller` 下对应的 Helper(聊天走 `RelayTextHelper`)。
  失败后按 `shouldRetry()` 决定是否换渠道重试,最多 `config.RetryTimes` 次,
  重试时调用 `CacheGetRandomSatisfiedChannel(group, model, ignoreFirstPriority=true)` 选用低优先级渠道。
- `controller/relay.go:105-122 → shouldRetry()`:
  重试条件:429 / 5xx 重试,400 与 2xx 不重试。

### 6. 文本中继主流程

- `relay/controller/text.go:25-88 → RelayTextHelper()`:
  1. 解析并校验请求体(`getAndValidateTextRequest`)
  2. 应用渠道 `ModelMapping` 做模型名替换
  3. 注入渠道级 `SystemPrompt`(若有)
  4. 取模型倍率 + 分组倍率(`billing/ratio`)
  5. **预扣费**(`preConsumeQuota`),按估算的 prompt token 数预先冻结额度
  6. `relay.GetAdaptor(apiType)` 拿到对应厂商 adapter
  7. `adaptor.ConvertRequest()` 把 OpenAI 协议转成上游协议
  8. `adaptor.DoRequest()` 发起上游 HTTP 调用
  9. `adaptor.DoResponse()` 解析响应(内部按 `meta.IsStream` 分流)
  10. **后扣费**(`postConsumeQuota`),按真实 usage 结算差额并写日志

### 7. Adapter 接口

- `relay/adaptor/interface.go:11-21 → Adaptor`:
  ```go
  type Adaptor interface {
      Init(meta *meta.Meta)
      GetRequestURL(meta *meta.Meta) (string, error)
      SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error
      ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error)
      ConvertImageRequest(request *model.ImageRequest) (any, error)
      DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error)
      DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (usage *model.Usage, err *model.ErrorWithStatusCode)
      GetModelList() []string
      GetChannelName() string
  }
  ```

### 8. 上游请求通用辅助

- `relay/adaptor/common.go:21-52 → DoRequestHelper()`:
  组装完整 URL、写入请求头,调 `client.HTTPClient.Do()`,
  并把 `http.Response` 透传回 adapter 的 `DoResponse`。

### 9. 响应处理(非流式 vs 流式)

非流式:
- `relay/adaptor/<vendor>/main.go → Handler()`:
  一次性读完 body,反序列化为 OpenAI 兼容响应,返回 `*model.Usage`。

**[流式 SSE]**:
- `relay/adaptor/<vendor>/main.go → StreamHandler()`(以 anthropic 为例,
  `relay/adaptor/anthropic/main.go:249-336`):
  1. `common.SetEventStreamHeaders(c)` 设置 `text/event-stream` 头
  2. `bufio.Scanner` 按行读 SSE
  3. 解析 `data: {...}` 事件,按事件类型(message_start / content_block_delta / message_delta / message_stop)分支
  4. **累加 token**:`usage.PromptTokens += meta.Usage.InputTokens; usage.CompletionTokens += meta.Usage.OutputTokens`
  5. `StreamResponseClaude2OpenAI()` 把 Claude 事件转成 OpenAI SSE chunk
  6. `render.ObjectData(c, response)` 实时刷给客户端
  7. 末尾返回累计后的 `*model.Usage` 给上层做计费

### 10. 后扣费与日志

- `relay/controller/helper.go:97-141 → postConsumeQuota()`(异步 goroutine):
  ```go
  completionRatio := billingratio.GetCompletionRatio(model, channelType)
  quota = int64(math.Ceil((float64(prompt) + float64(completion)*completionRatio) * ratio))
  delta := quota - preConsumedQuota
  model.PostConsumeTokenQuota(tokenId, delta)
  model.CacheUpdateUserQuota(userId)
  model.RecordConsumeLog(...)
  model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
  model.UpdateChannelUsedQuota(channelId, quota)
  ```

---

## 三、计费扣费逻辑

### 余额存储

| 主体 | 表 | 字段 | 文件 |
|------|------|------|------|
| 用户余额 | `users` | `Quota`(int64,剩余) / `UsedQuota`(int64,累计已用) / `RequestCount` | `model/user.go:34-54` |
| Token 余额 | `tokens` | `RemainQuota`(int64) / `UsedQuota` / `UnlimitedQuota`(bool) | `model/token.go:23-37` |
| 渠道用量 | `channels` | `UsedQuota`(int64) | `model/channel.go:20-41` |

### 预扣 vs 后扣

**采用"预扣 + 后结算"两段式**:

- **预扣**:`relay/controller/helper.go:68-95 → preConsumeQuota()`
  - 用 `openai.CountTokenMessages()` 估算 prompt tokens,乘以倍率得到一个预扣额度
  - 调 `model.CacheGetUserQuota` 拿余额,判断够不够
  - 调 `model.CacheDecreaseUserQuota(userId, q)` 在 Redis 原子减(避免抢锁)
  - 调 `model.PreConsumeTokenQuota(tokenId, q)`(`model/token.go:217-280`):
    检查 Token / User 余额,若用户余额接近阈值则异步发提醒邮件,然后
    `DecreaseTokenQuota` + `DecreaseUserQuota`。

- **后扣**:`model/token.go:282-300 → PostConsumeTokenQuota(tokenId, delta)`
  - `delta = 实际quota - 预扣quota`,可正可负(负代表退还)
  - 同时调整 User 和 Token 两侧余额
  - 然后在 `relay/billing/billing.go:23-52 → PostConsumeQuota()` 异步写 `logs` 并更新 channel 用量

### 事务与锁

- **普通请求路径不使用 DB 事务**,依赖"预扣"作为悲观占位,后扣只是结算差额。
- **兑换码兑换**走显式事务 + `SELECT FOR UPDATE`:
  `model/redemption.go:54-90 → Redeem()`:
  ```go
  err = DB.Transaction(func(tx *gorm.DB) error {
      err := tx.Set("gorm:query_option", "FOR UPDATE").Where(...).First(redemption).Error
      // 更新 redemption.Status = used,IncreaseUserQuota
  })
  ```
- **批量更新优化**:`BATCH_UPDATE_ENABLED=true` 时,`IncreaseUserQuota / DecreaseUserQuota / UpdateUserUsedQuotaAndRequestCount`(`model/user.go:374-432`)不立即写库,而是合并到队列,定时 flush,减少高并发下 DB 压力。

### 流式 token 累加

- 在 `StreamHandler` 内循环里把上游每个 chunk 的 usage 累加到本地 `model.Usage` 变量(见上文 anthropic 示例 line 287-290)。
- 结束时把累计 `Usage` 返回给 `RelayTextHelper`,由 `postConsumeQuota` 据此结算。
- 部分上游(如老 OpenAI completions 流)不返回 usage,使用 `openai.CountTokenText()` 对累计的 response 文本兜底估算。

---

## 四、渠道路由

### 选渠道

- 入口在分发中间件 `middleware/distributor.go`,实际查询走两条路:

  - **内存缓存路径**(`MEMORY_CACHE_ENABLED=true`):
    `model/cache.go:227-255 → CacheGetRandomSatisfiedChannel(group, model, ignoreFirstPriority)`:
    启动 + 定时 `SYNC_FREQUENCY` 同步,构建 `group2model2channels` 映射,按 `Priority` 降序。
    查询时:取出该 group×model 的渠道列表 → 找出最高 priority → 在最高优先级内随机一个;
    若 `ignoreFirstPriority=true`(重试时)则跳过最高优先级,从次优开始随机。

  - **直查 DB 路径**:
    `model/ability.go:22-51 → GetRandomSatisfiedChannel(group, model, ignoreFirstPriority)`:
    通过 `abilities` 表(group/model/channel_id/priority/enabled 复合主键)做子查询取最高 priority 再随机。

### 失败重试 / failover

- `controller/relay.go:65-91`:`Relay()` 主循环,失败后调
  `CacheGetRandomSatisfiedChannel(group, originalModel, i != retryTimes)` 选择**不同**渠道。
- 跳过上次失败的同一个渠道(`channel.Id == lastFailedChannelId` 时 continue)。
- 重试上限 = `config.RetryTimes`(Option 表 `RetryTimes`,默认 0/3,可在后台调)。

### 渠道权重(Weight)

- `Channel.Weight uint` 字段存在(`model/channel.go`),但**未在代码中找到按 weight 加权随机的实现**;
  实际选择是"按 Priority 分桶 → 桶内等概率随机"。Weight 当前为预留字段,
  二次开发若要做加权,需要改 `CacheGetRandomSatisfiedChannel` / `GetRandomSatisfiedChannel`。

### 渠道健康监控

- `monitor/` 目录提供 `Emit(channelId, success)` 记录最近 N 次结果,
  `ShouldDisableChannel(err, statusCode)` 判断是否要自动禁用渠道;
  当 `ENABLE_METRIC=true` 时,失败率超过阈值的渠道会被置为状态 3(自动禁用)。
- `controller/channel-test.go → AutomaticallyTestChannels(frequency)` 周期性测试被禁渠道,通过则恢复。

---

## 五、上游适配器

### 适配器目录

每家上游一个子目录,位于 `relay/adaptor/<vendor>/`,常见的:

`openai/` `anthropic/` `gemini/` `geminiv2/` `baidu/` `baiduv2/` `ali/` `alibailian/`
`deepseek/` `zhipu/` `tencent/` `xunfei/` `xunfeiv2/` `aws/`(Bedrock Claude) `cohere/`
`groq/` `mistral/` `xai/` `lingyiwanwu/` `stepfun/` `moonshot/` `minimax/` `doubao/`
`cloudflare/` `palm/` `ollama/` `replicate/` `coze/` `proxy/` ...

每个目录基本包含:

- `adaptor.go` 实现 `Adaptor` 接口
- `model.go` 上游请求/响应结构体
- `main.go` `ConvertRequest` / `Handler` / `StreamHandler`
- 部分有 `constants.go` 模型列表

### 新增一家上游需要改的文件

1. **新建** `relay/adaptor/<vendor>/`,实现:
   - `adaptor.go`(实现接口 9 个方法,可参考 `anthropic/adaptor.go`)
   - `model.go`(请求体、响应体结构)
   - `main.go`(`ConvertRequest`、`Handler`、`StreamHandler`)
   - `constants.go`(模型名列表 `ModelList`)
2. **注册渠道类型**:`relay/channeltype/define.go` 加常量、`relay/channeltype/url.go` 给默认 BaseURL、`relay/apitype/define.go` 加 ApiType。
3. **接入适配器工厂**:`relay/adaptor.go`(或 `relay/relay.go`)里的 `GetAdaptor(apiType)` switch 加分支。
4. **倍率**:`relay/billing/ratio/model_ratio.go` 给模型加默认倍率与 `completion_ratio.go` 比率。
5. **前端渠道选择**(可选):`web/src/constants/channel.constants.js`(或对应主题目录)加渠道选项,
   以便后台"新增渠道"下拉框出现该厂商。

### 各家 token 计数差异

- **OpenAI 系**:走 `relay/adaptor/openai/token.go` 的 `CountTokenMessages` / `CountTokenText`,
  使用 `pkoukk/tiktoken-go`,支持离线缓存目录 `TIKTOKEN_CACHE_DIR`。
- **Claude(Anthropic)**:没有官方 tiktoken 编码。one-api 的处理是:
  - 预扣阶段用 OpenAI tiktoken 对消息文本做**近似估算**(同 `CountTokenMessages`)。
  - 实际计费**信任 Claude 返回的 `usage.input_tokens` / `usage.output_tokens`**,
    在 `StreamHandler` / `Handler` 中读取后覆盖到 `model.Usage`。
- **Gemini / Baidu / Ali / Zhipu 等**:同样优先用上游返回的 usage;
  若不返回,则 fallback 到对响应文本调用 `openai.CountTokenText()` 估算。
- **AWS Bedrock Claude**:直接复用 anthropic 的转换逻辑 + AWS SigV4 签名。

---

## 六、数据库 Schema

迁移入口:`model/main.go:111-163 → InitDB() / migrateDB()`,使用 GORM `AutoMigrate`。
支持 MySQL / PostgreSQL / SQLite(由 `SQL_DSN` 决定),日志可选独立库 `LOG_SQL_DSN`。

### users(`model/user.go:34-54`)

| 字段 | 类型 | 含义 |
|------|------|------|
| Id | int | 主键 |
| Username | varchar(12) UNIQUE | 登录名 |
| Password | text | bcrypt 密码 |
| DisplayName | varchar(20) | 显示名 |
| Role | int | 0 游客 / 1 普通 / 10 管理员 / 100 root |
| Status | int | 1 启用 / 2 禁用 / 3 删除 |
| Email | varchar(50) | 邮箱 |
| **Quota** | bigint | **剩余额度** |
| UsedQuota | bigint | 历史总用量 |
| RequestCount | int | 请求计数 |
| Group | varchar(32) | 用户分组(决定可用渠道) |
| AffCode / InviterId | - | 邀请关系 |
| GitHubId / WeChatId / LarkId / OidcId | - | 第三方登录绑定 |

### tokens(`model/token.go:23-37`)

| 字段 | 含义 |
|------|------|
| Id | 主键 |
| UserId | 所属用户 |
| Key | char(48) UNIQUE,即 `sk-xxx` |
| Status | 1 启用 / 2 禁用 / 3 过期 / 4 耗尽 |
| Name | Token 名 |
| ExpiredTime | -1 永不过期 |
| **RemainQuota** | Token 内剩余额度 |
| UnlimitedQuota | 是否不限额 |
| UsedQuota | 已用 |
| Models | 允许调用的模型(逗号分隔) |
| Subnet | 允许的客户端 CIDR |

### channels(`model/channel.go:20-41`)

| 字段 | 含义 |
|------|------|
| Id / Name / Type | 渠道 ID / 名称 / 厂商类型 |
| Key | API Key(可逗号分隔多 key 轮询,部分渠道用 JSON) |
| Status | 1 启用 / 2 手动禁用 / 3 自动禁用 |
| BaseURL | 覆盖默认上游地址 |
| Models | 该渠道开放的模型 |
| Group | 服务的用户分组(逗号分隔) |
| Priority | 优先级,大者优先 |
| Weight | 权重(**预留,目前未生效**) |
| ModelMapping | JSON,把请求模型重命名为上游真实模型 |
| Config | JSON,放厂商特有配置(Region、API Version 等) |
| SystemPrompt | 渠道级强制 system prompt |
| UsedQuota / Balance / ResponseTime / TestTime | 统计与监控字段 |

### abilities(`model/ability.go:14-20`)

复合主键 (Group, Model, ChannelId),作为"分组×模型→渠道"的倒排索引,
分发中间件靠它做 O(1) 选渠道。字段:Enabled、Priority。

### logs(`model/log.go:15-32`)

| 字段 | 含义 |
|------|------|
| UserId / Username / TokenName | 谁 |
| CreatedAt | 时间(bigint timestamp) |
| Type | 1 充值 / 2 消费 / 3 管理 / 4 系统 / 5 测试 |
| Content | 摘要文本 |
| ModelName | 模型 |
| PromptTokens / CompletionTokens | token 数 |
| Quota | 本次消耗额度 |
| ChannelId | 走的渠道 |
| RequestId / ElapsedTime / IsStream | 调试/审计字段 |

### redemptions(`model/redemption.go:20-30`)

兑换码:Key(char32 UNIQUE)、Quota(面值)、Status(1启/2禁/3已用)、RedeemedTime、UserId。

### options(`model/option.go:12-15`)

```go
type Option struct {
    Key   string `gorm:"primaryKey"`
    Value string
}
```

把 KV 配置(如 `ModelRatio` JSON、`GroupRatio` JSON、`RetryTimes`、`QuotaForNewUser` 等)
持久化到 DB,启动时 `InitOptionMap()` 全量加载到内存,后台修改即时同步。

---

## 七、配置项

### 读取机制

- 启动早期 `os.Getenv()` 直读关键环境变量(`common/config/config.go` 顶部)。
- 通用辅助:`common/env/helper.go:8-42 → Bool/Int/Float64/String`。
- 运行时配置(倍率、开关、文案等)由 `model/option.go` 从 `options` 表加载到 `config.OptionMap`,
  在管理后台改动会回写并广播。

### 10 个最常用的环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `SQL_DSN` | 空(用 SQLite) | 主库 DSN(MySQL/PG/SQLite) |
| `REDIS_CONN_STRING` | 空 | Redis 连接串,启用后开启分布式缓存与限流 |
| `SESSION_SECRET` | 随机 UUID | 多节点必须显式设置以共享 session |
| `FRONTEND_BASE_URL` | 空 | 从节点把前端请求重定向到主节点用 |
| `MEMORY_CACHE_ENABLED` | false | 启用内存渠道/用户缓存(高 QPS 建议开) |
| `SYNC_FREQUENCY` / `CHANNEL_UPDATE_FREQUENCY` | 600 | 缓存同步周期(秒) |
| `BATCH_UPDATE_ENABLED` | false | DB 写入批处理 |
| `BATCH_UPDATE_INTERVAL` | 5 | 批处理 flush 间隔(秒) |
| `GLOBAL_API_RATE_LIMIT` | 480 | 每 IP 在窗口内最大请求数 |
| `PORT` | 3000 | HTTP 端口 |

其它常见的(附带文件位置,详细以 `common/config/config.go` 为准):

- `NODE_TYPE` (master/slave)、`DEBUG` / `DEBUG_SQL` / `GIN_MODE`
- `CHANNEL_TEST_FREQUENCY` 自动巡检禁用渠道的频率
- `RELAY_TIMEOUT` 中继上游超时
- `TIKTOKEN_CACHE_DIR` 离线 tiktoken 缓存
- `ENABLE_METRIC` 启用渠道失败率监控
- `INITIAL_ROOT_TOKEN` / `INITIAL_ROOT_ACCESS_TOKEN` 首次启动注入 root 凭证
- 第三方登录:`GITHUB_CLIENT_ID/SECRET`、`OIDC_CLIENT_ID/SECRET`、`WECHAT_SERVER_ADDRESS/TOKEN`
- 邮件:`SMTP_SERVER` / `SMTP_PORT` / `SMTP_ACCOUNT` / `SMTP_FROM` / `SMTP_TOKEN`
- 验证码:`TURNSTILE_SECRET_KEY`

---

## 八、二次开发常用入口点

| 场景 | 改哪些文件 / 加在哪个环节 |
|------|--------------------------|
| **接入支付**(微信/支付宝/Stripe) | 新增 `controller/payment.go`(下单、回调);在 `router/api.go` 挂路由;回调成功后调用 `model.IncreaseUserQuota`(`model/user.go:374`) + `model.RecordTopupLog`(`model/log.go`);如要兑换码,参考 `model/redemption.go` 的事务写法。 |
| **内容审核**(输入/输出) | 入参审核加在 `relay/controller/text.go:RelayTextHelper` 解析 request 之后、`adaptor.DoRequest` 之前;响应审核可在 `adaptor.DoResponse` 之后(非流)或 `StreamHandler` 内对累计文本拦截;独立中间件可放在 `middleware/audit.go`,在 `router/relay.go` 中按需挂载。被拦截时使用 `openai.ErrorWrapper(err, "content_audit_failed", 403)` 返回统一错误格式。 |
| **自定义限流**(按用户/按模型/按 Token) | 在 `middleware/rate-limit.go` 仿照 `rateLimitFactory` 增加新 `UserModelRateLimit(userId, model)`,在 `router/relay.go` 的 `/v1` 路由组里追加 `Use(...)`;计数 key 命名 `rateLimit:UM<userId>_<model>`,后端走已有 Redis/内存切换逻辑。 |
| **Prometheus 监控** | 新增 `middleware/metrics.go`,定义 `CounterVec`(请求数)/`HistogramVec`(耗时)/`CounterVec`(token 消耗,labels=model/channel/user);在 `main.go` 注册 `prometheus.MustRegister(...)` 并 `server.GET("/metrics", gin.WrapH(promhttp.Handler()))`;token 计数在 `relay/billing/billing.go:PostConsumeQuota` 内 `Add(float64(quota))`。 |
| **新增一家上游模型** | (1) 新建 `relay/adaptor/<vendor>/{adaptor.go, model.go, main.go, constants.go}`;(2) `relay/channeltype/define.go` + `url.go` 注册类型与默认 BaseURL;(3) `relay/apitype/define.go` 加 ApiType;(4) `relay/adaptor.go` 的 `GetAdaptor` switch 加分支;(5) `relay/billing/ratio/model_ratio.go` + `completion_ratio.go` 补倍率;(6) 前端 `web/src/constants/channel.constants.js` 加下拉项。 |
| **改余额/计费公式** | 倍率配置:运行时改 Option 表的 `ModelRatio` / `GroupRatio` / `CompletionRatio` JSON(后台界面或 `controller/option.go`);**公式**本身在 `relay/controller/helper.go:postConsumeQuota` 那一行 `quota = ceil((prompt + completion*completionRatio) * ratio)`,要叠加 VIP 折扣、高峰加价、阶梯计费就改这里;余额读写函数集中在 `model/user.go:374-432`(Increase/Decrease/UsedQuota)与 `model/token.go:173-300`。 |

### 其他常见扩展点速查

| 需求 | 切入点 |
|------|--------|
| 注册流程增强(邀请码/手机号) | `controller/auth/` 下的 `Register()` |
| Token 创建限制 | `controller/token.go:CreateToken()` |
| 渠道自检逻辑 | `controller/channel-test.go:TestChannel()` |
| 日志结构扩展 | `model/log.go:RecordConsumeLog()` + Log 结构体 |
| 统一错误格式/国际化 | `relay/adaptor/openai/util.go:ErrorWrapper` |
| 缓存策略 | `model/cache.go`(Token / User / Channel 三套) |
| 黑名单 | `common/blacklist/` |

---

## 附录:文档使用说明

- 行号会随代码改动漂移,以**函数名**为准定位。
- 二次开发完成后,如调用链或关键文件路径发生结构性变化,请同步更新本文。
- 本文档刻意不覆盖前端(`web/`)细节,前端二次开发请直接读对应主题目录的 README。
