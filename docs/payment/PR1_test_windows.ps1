#Requires -Version 5.1
<#
.SYNOPSIS
    PR #1 (/api/internal/topup) 端到端测试脚本 - Windows PowerShell 版

.DESCRIPTION
    自动跑五个场景:
      1. happy path 充值
      2. 幂等重放
      3. 签名失败(401)
      4. 退款成功
      5. 余额不足(409, 事务回滚)

    每个场景前后都查一遍数据库,打印 internal_topup_records 行数 和 users.quota。

.PARAMETER BaseUrl
    one-api 的根 URL。默认 http://localhost:3000

.PARAMETER Secret
    INTERNAL_API_SECRET 的值。默认从环境变量 $env:INTERNAL_API_SECRET 读。
    必须和 one-api 进程跑的时候那个一模一样,否则 5 个场景全 401。

.PARAMETER DbPath
    SQLite 文件在 Windows 宿主机上的路径(host-side)。
    docker-compose.yml 把容器内 /data 挂到宿主 ./data/oneapi,
    所以默认 .\data\oneapi\one-api.db。如果你用的是 MySQL,
    DB 验证步骤会被跳过 - 但 HTTP 测试仍然有效。

.PARAMETER ContainerName
    one-api 容器名。当宿主机没装 sqlite3.exe 时,
    会回退到 `docker exec $ContainerName sqlite3 ...`。
    多数官方镜像不带 sqlite3 二进制,所以推荐在宿主装 sqlite3.exe。

.PARAMETER UserId
    跑场景 1/2/4/5 的用户 ID。默认 1 (root)。

.PARAMETER TopupAmount
    场景 1 充值多少 quota。默认 1000。

.PARAMETER RefundAmount
    场景 4 退多少 quota。必须 <= TopupAmount。默认 500。

.PARAMETER LargeRefundAmount
    场景 5 故意填一个 > 用户余额的退款额。默认 999_999_999_999_999
    (~999 万亿 quota,远大于 root 默认 500 万亿,触发 insufficient_balance)。

.EXAMPLE
    $env:INTERNAL_API_SECRET = "your-32-char-secret-here-xxxxxxxxxxx"
    .\docs\payment\PR1_test_windows.ps1

.EXAMPLE
    .\docs\payment\PR1_test_windows.ps1 -BaseUrl http://192.168.1.10:3000 -UserId 2
#>

[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:3000",
    [string]$Secret = $env:INTERNAL_API_SECRET,
    [string]$DbPath = ".\data\oneapi\one-api.db",
    [string]$ContainerName = "one-api",
    [int]$UserId = 1,
    [int]$TopupAmount = 1000,
    [int]$RefundAmount = 500,
    [long]$LargeRefundAmount = 999999999999999
)

$ErrorActionPreference = "Stop"

# ---------------------------------------------------------------------
# 防呆检查
# ---------------------------------------------------------------------
if ([string]::IsNullOrWhiteSpace($Secret)) {
    Write-Host "ERROR: INTERNAL_API_SECRET 没设置。" -ForegroundColor Red
    Write-Host "用法:`n  `$env:INTERNAL_API_SECRET = '<和 one-api 启动时一致的值>'" -ForegroundColor Yellow
    exit 1
}
if ($Secret.Length -lt 32) {
    Write-Host "WARNING: 密钥短于 32 字符,one-api 中间件会拒绝(503 internal_auth_not_configured)。" -ForegroundColor Yellow
}

# 生成本次运行独有的 order_no 前缀,避免和上一次跑的脏数据冲突
$RunTag    = (Get-Date -Format "yyyyMMddHHmmss")
$OrderPay  = "IDR{0}TOPUP01" -f $RunTag    # 场景 1/2/4 共享
$OrderRefBad = "IDR{0}INSUF1" -f $RunTag   # 场景 5

# ---------------------------------------------------------------------
# 加解密 / 签名工具
# ---------------------------------------------------------------------
function ConvertTo-HexLower {
    param([byte[]]$Bytes)
    -join ($Bytes | ForEach-Object { $_.ToString("x2") })
}

function Get-Sha256Hex {
    param([string]$Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        ConvertTo-HexLower $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($Text))
    } finally {
        $sha.Dispose()
    }
}

function Get-HmacSha256Hex {
    param([string]$Key, [string]$Message)
    $hmac = New-Object System.Security.Cryptography.HMACSHA256
    $hmac.Key = [System.Text.Encoding]::UTF8.GetBytes($Key)
    try {
        ConvertTo-HexLower $hmac.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($Message))
    } finally {
        $hmac.Dispose()
    }
}

# 严格按 PR #1 的签名格式构造:
#   canonical = timestamp + "\n" + METHOD + "\n" + path + "\n" + hex(sha256(body))
#   X-Internal-Signature = hex(hmac_sha256(secret, canonical))
function New-InternalAuthHeaders {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Body,           # 原始字符串 - 必须和实际发送的 body 字节一致
        [string]$Secret,
        [long]  $TimestampOverride = 0
    )
    if ($TimestampOverride -gt 0) {
        $ts = $TimestampOverride
    } else {
        $ts = [int64]([DateTimeOffset]::UtcNow.ToUnixTimeSeconds())
    }
    $bodyHash  = Get-Sha256Hex -Text $Body
    $canonical = "{0}`n{1}`n{2}`n{3}" -f $ts, $Method.ToUpper(), $Path, $bodyHash
    $sig = Get-HmacSha256Hex -Key $Secret -Message $canonical
    return @{
        "X-Internal-Timestamp" = "$ts"
        "X-Internal-Signature" = $sig
        "Content-Type"         = "application/json"
    }
}

# 调 /api/internal/topup,返回 @{Status, Body(原始字符串), Json(解析后哈希表)}
function Invoke-InternalTopup {
    param(
        [hashtable]$Payload,
        [string]   $Secret,
        [switch]   $TamperSignature   # 故意改坏一个字符,用于场景 3
    )
    # 关键:用 -Compress 输出紧凑 JSON,然后立即把字符串和字节锁死,
    # 后面发出的 body 必须和参与签名的字符串一字不差。
    $bodyStr   = $Payload | ConvertTo-Json -Compress -Depth 5
    $bodyBytes = [System.Text.Encoding]::UTF8.GetBytes($bodyStr)
    $headers   = New-InternalAuthHeaders -Method "POST" -Path "/api/internal/topup" -Body $bodyStr -Secret $Secret

    if ($TamperSignature) {
        # 翻转签名末位字符,确保 hex 仍合法但 HMAC 不匹配
        $orig = $headers["X-Internal-Signature"]
        $last = $orig.Substring($orig.Length - 1)
        $flip = if ($last -eq "0") { "1" } else { "0" }
        $headers["X-Internal-Signature"] = $orig.Substring(0, $orig.Length - 1) + $flip
    }

    $url = "$BaseUrl/api/internal/topup"
    try {
        # PS 5.1 没有 -SkipHttpErrorCheck,所以非 2xx 会抛 - 用 try/catch 接住
        $resp = Invoke-WebRequest -Uri $url -Method Post -Headers $headers `
                                  -Body $bodyBytes -UseBasicParsing -ErrorAction Stop
        $status = [int]$resp.StatusCode
        $bodyOut = $resp.Content
    } catch {
        $excResp = $_.Exception.Response
        if ($null -eq $excResp) { throw }
        $status = [int]$excResp.StatusCode
        # 优先用 ErrorDetails.Message (PS 5.1),回退到读 Response Stream
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $bodyOut = $_.ErrorDetails.Message
        } else {
            try {
                $stream = $excResp.GetResponseStream()
                $reader = New-Object System.IO.StreamReader($stream)
                $bodyOut = $reader.ReadToEnd()
                $reader.Dispose()
            } catch { $bodyOut = "" }
        }
    }
    $jsonObj = $null
    try { $jsonObj = $bodyOut | ConvertFrom-Json } catch {}
    return @{ Status = $status; Body = $bodyOut; Json = $jsonObj }
}

# ---------------------------------------------------------------------
# SQLite 查询 - 优先用宿主 sqlite3.exe,回退到 docker exec,都没有就跳过
# ---------------------------------------------------------------------
function Test-HostSqlite { try { sqlite3 -version 2>$null | Out-Null; $LASTEXITCODE -eq 0 } catch { $false } }
function Test-DockerExec { try { docker version 2>$null | Out-Null; $LASTEXITCODE -eq 0 } catch { $false } }

function Invoke-Sqlite {
    param([string]$Sql)
    # 1) host-side sqlite3 + 已挂载的 db 文件
    if ((Test-HostSqlite) -and (Test-Path $DbPath)) {
        return (& sqlite3 -separator "|" $DbPath $Sql 2>&1)
    }
    # 2) docker exec 容器内 sqlite3 (多数官方镜像没装)
    if (Test-DockerExec) {
        $out = & docker exec $ContainerName sh -c "command -v sqlite3 >/dev/null && sqlite3 -separator '|' /data/one-api.db `"$Sql`"" 2>&1
        if ($LASTEXITCODE -eq 0) { return $out }
    }
    return $null
}

function Get-DbState {
    param([string]$OrderNo, [int]$UserIdLocal)
    $state = @{
        UserQuota  = "(unknown)"
        TopupCount = "(unknown)"
        TopupRows  = @()
        Available  = $false
    }
    $q1 = Invoke-Sqlite "SELECT quota FROM users WHERE id=$UserIdLocal;"
    $q2 = Invoke-Sqlite "SELECT COUNT(*) FROM internal_topup_records WHERE order_no='$OrderNo';"
    $q3 = Invoke-Sqlite "SELECT id,order_no,action,user_id,quota,new_balance,created_at FROM internal_topup_records WHERE order_no='$OrderNo';"
    if ($null -ne $q1) {
        $state.Available  = $true
        $state.UserQuota  = ($q1 | Out-String).Trim()
        $state.TopupCount = ($q2 | Out-String).Trim()
        $state.TopupRows  = @($q3 | ForEach-Object { $_.ToString() })
    }
    return $state
}

function Write-DbState {
    param([string]$Label, [hashtable]$State)
    if (-not $State.Available) {
        Write-Host "    DB[$Label]: (skipped - 宿主和容器都拿不到 sqlite3,或 DbPath 不对)" -ForegroundColor DarkGray
        return
    }
    Write-Host "    DB[$Label]: users.quota=$($State.UserQuota), internal_topup_records 行数=$($State.TopupCount)" -ForegroundColor DarkGray
    foreach ($r in $State.TopupRows) {
        Write-Host "      row: $r" -ForegroundColor DarkGray
    }
}

# ---------------------------------------------------------------------
# 测试结果聚合
# ---------------------------------------------------------------------
$Results = New-Object System.Collections.ArrayList

function Add-Result {
    param([bool]$Pass, [string]$Name, [string]$Reason = "")
    $tag = if ($Pass) { "PASS" } else { "FAIL" }
    $color = if ($Pass) { "Green" } else { "Red" }
    Write-Host "[$tag] $Name" -ForegroundColor $color
    if ($Reason) { Write-Host "    -> $Reason" -ForegroundColor $color }
    [void]$Results.Add([pscustomobject]@{ Pass = $Pass; Name = $Name; Reason = $Reason })
}

function Write-Resp {
    param($Resp)
    Write-Host "    HTTP $($Resp.Status)" -ForegroundColor Cyan
    Write-Host "    Body: $($Resp.Body)" -ForegroundColor Cyan
}

# ---------------------------------------------------------------------
# 场景 1: happy path
# ---------------------------------------------------------------------
function Test-Scenario1 {
    Write-Host "`n==== 场景 1: happy path - 充值 $TopupAmount 给 user_id=$UserId ====" -ForegroundColor White
    $before = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "before" $before

    $payload = @{ action="topup"; order_no=$OrderPay; user_id=$UserId; quota=$TopupAmount; remark="scenario1 happy path" }
    $resp = Invoke-InternalTopup -Payload $payload -Secret $Secret
    Write-Resp $resp

    $after = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "after" $after

    $pass = ($resp.Status -eq 200) -and ($resp.Json.success -eq $true) `
            -and ($resp.Json.data.idempotent_replay -eq $false) `
            -and ($resp.Json.data.quota -eq $TopupAmount)
    $reason = if ($pass) { "new_balance=$($resp.Json.data.new_balance)" } else { "期望 200 + success + idempotent_replay=false, 实际见上方" }
    Add-Result $pass "场景1: happy path 充值" $reason
}

# ---------------------------------------------------------------------
# 场景 2: 幂等重放(同 order_no 再发一次,余额不能再增)
# ---------------------------------------------------------------------
function Test-Scenario2 {
    Write-Host "`n==== 场景 2: 幂等重放 - 把场景1的请求原样再发一次 ====" -ForegroundColor White
    $before = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "before" $before

    $payload = @{ action="topup"; order_no=$OrderPay; user_id=$UserId; quota=$TopupAmount; remark="scenario1 happy path" }
    $resp = Invoke-InternalTopup -Payload $payload -Secret $Secret
    Write-Resp $resp

    $after = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "after" $after

    $balanceUnchanged = $true
    if ($before.Available -and $after.Available) {
        $balanceUnchanged = ($before.UserQuota -eq $after.UserQuota)
    }
    $pass = ($resp.Status -eq 200) -and ($resp.Json.success -eq $true) `
            -and ($resp.Json.data.idempotent_replay -eq $true) -and $balanceUnchanged
    $reason = if ($pass) { "余额未变 + idempotent_replay=true" }
              elseif (-not $balanceUnchanged) { "DB 余额改变了: before=$($before.UserQuota) after=$($after.UserQuota)" }
              else { "期望 idempotent_replay=true,实际见上方" }
    Add-Result $pass "场景2: 幂等重放" $reason
}

# ---------------------------------------------------------------------
# 场景 3: 签名失败 - 故意篡改签名末位
# ---------------------------------------------------------------------
function Test-Scenario3 {
    Write-Host "`n==== 场景 3: 签名失败 - 用故意改坏的签名调一次新订单 ====" -ForegroundColor White
    $tamperedOrder = "IDR{0}BADSIG" -f $RunTag
    $payload = @{ action="topup"; order_no=$tamperedOrder; user_id=$UserId; quota=1; remark="scenario3 tampered" }
    $resp = Invoke-InternalTopup -Payload $payload -Secret $Secret -TamperSignature

    Write-Resp $resp

    $after = Get-DbState -OrderNo $tamperedOrder -UserIdLocal $UserId
    Write-DbState "after-should-have-no-row" $after

    $noRow = $true
    if ($after.Available) { $noRow = ($after.TopupCount -eq "0") }

    $pass = ($resp.Status -eq 401) `
            -and (($resp.Json.code -eq "signature_mismatch") -or ($resp.Json.code -eq "invalid_signature_format")) `
            -and $noRow
    $reason = if ($pass) { "401 + code=$($resp.Json.code) + 无 DB 副作用" } else { "期望 401 signature_mismatch,实际见上方" }
    Add-Result $pass "场景3: 签名失败" $reason
}

# ---------------------------------------------------------------------
# 场景 4: 退款 - 对场景1的订单发 refund (同 order_no, 不同 action 复合幂等)
# ---------------------------------------------------------------------
function Test-Scenario4 {
    Write-Host "`n==== 场景 4: 退款 - 对场景1的 order_no 发 action=refund 退 $RefundAmount ====" -ForegroundColor White
    $before = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "before" $before

    $payload = @{ action="refund"; order_no=$OrderPay; user_id=$UserId; quota=$RefundAmount; remark="scenario4 refund" }
    $resp = Invoke-InternalTopup -Payload $payload -Secret $Secret
    Write-Resp $resp

    $after = Get-DbState -OrderNo $OrderPay -UserIdLocal $UserId
    Write-DbState "after" $after

    $balanceDropped = $true
    if ($before.Available -and $after.Available) {
        try {
            $b = [int64]$before.UserQuota
            $a = [int64]$after.UserQuota
            $balanceDropped = ($b - $a -eq $RefundAmount)
        } catch { $balanceDropped = $false }
    }
    $pass = ($resp.Status -eq 200) -and ($resp.Json.success -eq $true) `
            -and ($resp.Json.data.action -eq "refund") `
            -and ($resp.Json.data.quota -eq -$RefundAmount) `
            -and $balanceDropped
    $reason = if ($pass) { "余额减少 $RefundAmount,signed quota=$($resp.Json.data.quota)" } else { "见上方" }
    Add-Result $pass "场景4: 退款" $reason
}

# ---------------------------------------------------------------------
# 场景 5: 余额不足 - 故意退一个 > 用户余额的金额,期望 409 且事务回滚
# ---------------------------------------------------------------------
function Test-Scenario5 {
    Write-Host "`n==== 场景 5: 余额不足 - 退 $LargeRefundAmount,期望 409 + 事务回滚 ====" -ForegroundColor White
    $before = Get-DbState -OrderNo $OrderRefBad -UserIdLocal $UserId
    Write-DbState "before" $before

    $payload = @{ action="refund"; order_no=$OrderRefBad; user_id=$UserId; quota=$LargeRefundAmount; remark="scenario5 must fail" }
    $resp = Invoke-InternalTopup -Payload $payload -Secret $Secret
    Write-Resp $resp

    $after = Get-DbState -OrderNo $OrderRefBad -UserIdLocal $UserId
    Write-DbState "after-should-have-no-row" $after

    $balanceUnchanged = $true
    if ($before.Available -and $after.Available) {
        $balanceUnchanged = ($before.UserQuota -eq $after.UserQuota)
    }
    $noRow = $true
    if ($after.Available) { $noRow = ($after.TopupCount -eq "0") }

    $pass = ($resp.Status -eq 409) -and ($resp.Json.code -eq "insufficient_balance") `
            -and $balanceUnchanged -and $noRow
    $reason = if ($pass) { "409 insufficient_balance + 余额未变 + 幂等表无残留行" }
              elseif (-not $balanceUnchanged) { "BUG: 失败的退款改了余额: before=$($before.UserQuota) after=$($after.UserQuota)" }
              elseif (-not $noRow) { "BUG: 失败的退款在 internal_topup_records 留了行,会阻塞重试" }
              else { "期望 409 insufficient_balance,实际见上方" }
    Add-Result $pass "场景5: 余额不足回滚" $reason
}

# ---------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------
Write-Host "================================================================" -ForegroundColor White
Write-Host " PR #1 内部端点测试  -  base=$BaseUrl  user=$UserId  runTag=$RunTag" -ForegroundColor White
Write-Host "================================================================" -ForegroundColor White

# 启动时探测一下 SQLite 是否能拿到
$probe = Get-DbState -OrderNo "__probe__" -UserIdLocal $UserId
if (-not $probe.Available) {
    Write-Host "提示: 拿不到 SQLite (-DbPath '$DbPath' 不存在,且容器内可能没装 sqlite3)。" -ForegroundColor Yellow
    Write-Host "      场景判定仅依据 HTTP 响应,DB 交叉验证会被跳过。" -ForegroundColor Yellow
    Write-Host "      装 sqlite3.exe (choco install sqlite) 或修改 -DbPath 后重跑可启用。" -ForegroundColor Yellow
}

Test-Scenario1
Test-Scenario2
Test-Scenario3
Test-Scenario4
Test-Scenario5

# ---------------------------------------------------------------------
# 总结
# ---------------------------------------------------------------------
Write-Host "`n================================================================" -ForegroundColor White
$passCount = ($Results | Where-Object Pass).Count
$failCount = ($Results | Where-Object { -not $_.Pass }).Count
$summaryColor = if ($failCount -eq 0) { "Green" } else { "Red" }
Write-Host (" Summary: PASS={0}  FAIL={1}  (total {2})" -f $passCount, $failCount, $Results.Count) -ForegroundColor $summaryColor
foreach ($r in $Results) {
    $tag = if ($r.Pass) { "PASS" } else { "FAIL" }
    $c   = if ($r.Pass) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1}" -f $tag, $r.Name) -ForegroundColor $c
}
Write-Host "================================================================" -ForegroundColor White

if ($failCount -gt 0) { exit 1 } else { exit 0 }

<#
.NOTES
    人工核查清单(脚本无法替代):

    [手动] 时间戳防重放:本脚本一直用 now() 签名,不会触发 timestamp_out_of_window。
           手动验证:把电脑系统时间往前/后调 5 分钟,再跑场景 1,应该收到
           401 + code=timestamp_out_of_window。验证完记得改回系统时间。

    [手动] 中间件未配密钥:停 one-api,清掉 INTERNAL_API_SECRET 重启,跑场景 1,
           应该收到 503 + code=internal_auth_not_configured。

    [手动] body 篡改:脚本只篡改签名,没单独测"签名对、body 改了"这条路径。
           想测的话:用 curl 手动构造,签名用 body A,但 -d 传 body B,
           期望 401 signature_mismatch。
           (单元测试已覆盖这条,见 middleware/internal_auth_test.go::TestInternalAuth_TamperedBody)

    [手动] 用户不存在:跑命令时把 -UserId 设成一个绝对不存在的 ID,
           应该收到 404 + code=user_not_found。

    [手动] 检查 logs 表:充值成功后,SQLite 跑:
              SELECT * FROM logs WHERE type=1 ORDER BY id DESC LIMIT 5;
           应该看到 type=1 (LogTypeTopup) 的新记录,content 里带订单号。

    [限制] 场景 5 假定用户 ID=1 (root) 余额 < $LargeRefundAmount (999 万亿)。
           one-api 给 root 的默认 quota 是 500 万亿,本脚本默认值能稳定触发 409。
           如果你换了用户或把 root 余额 topup 得超过 999 万亿,要相应改 -LargeRefundAmount。

    [限制] 容器内大概率没装 sqlite3,所以 DB 交叉验证强依赖宿主有 sqlite3.exe。
           Windows 装法:  choco install sqlite   或   scoop install sqlite
#>
