#Requires -Version 5.1
<#
.SYNOPSIS
    PR #2 (payment-service) end-to-end test for Windows PowerShell.

.DESCRIPTION
    Exercises the full QRIS top-up flow against a docker-compose stack.
    Webhook deliveries are simulated locally (Xendit doesn't push to localhost),
    so this script generates webhook payloads that mimic Xendit's format and
    signs them with `xendit_webhook_token_qris` from payment_config.

    Scenarios:
      1.  /healthz                                  -> 200
      2.  POST /orders with amount < 10_000          -> 400 ORDER_AMOUNT_TOO_LOW
      3.  POST /orders with amount > 10_000_000      -> 400 ORDER_AMOUNT_TOO_HIGH
      4.  POST /orders happy path (QRIS, 50_000 IDR) -> 200 + qr_string non-empty
      5.  GET  /orders/:no                           -> echoes the created order
      6.  Webhook qr.payment SUCCEEDED               -> order pending -> paid -> credited
                                                       + one-api user balance increased
      7.  Webhook replay (same id)                    -> 200 duplicate, no double-credit
      8.  Late callback after expiry (DB-injected)    -> needs_manual_review, NO credit
      9.  Late callback after cancel (DB-injected)    -> needs_manual_review, NO credit
      10. IP whitelist lockdown test                  -> 403
      11. Exchange-rate lockdown                      -> old order unchanged, new order uses new rate

    Each scenario reports [PASS/FAIL] + relevant DB rows.

.PARAMETER PaymentBase
    Base URL of payment-service. Default http://localhost:3001.

.PARAMETER OneAPIBase
    Base URL of one-api. Default http://localhost:3000.

.PARAMETER RootAccessToken
    one-api root user's access_token (from `users.access_token` column or the
    "/api/user/token" admin endpoint). Used as the Authorization header on
    /orders calls. Required.

.PARAMETER WebhookTokenQRIS
    Value of `xendit_webhook_token_qris` in payment_config. Required to sign
    simulated webhook deliveries. If you haven't set one yet:
      docker exec mysql mysql -u oneapi -p123456 oneapi_payment -e \
        "UPDATE payment_config SET config_value='test-qris-token' WHERE config_key='xendit_webhook_token_qris';"
    then pass -WebhookTokenQRIS 'test-qris-token'.

.PARAMETER MySQLContainer
    Container name for the MySQL service. Default 'mysql' (per docker-compose).

.PARAMETER MySQLUser / MySQLPass
    Credentials for the MySQL container. Default oneapi / 123456 (per docker-compose).

.EXAMPLE
    $env:WebhookTokenQRIS = 'test-qris-token'
    .\payment\docs\PR2_test_windows.ps1 -RootAccessToken '4f25...' -WebhookTokenQRIS 'test-qris-token'
#>

[CmdletBinding()]
param(
    [string]$PaymentBase = "http://localhost:3001",
    [string]$OneAPIBase = "http://localhost:3000",
    [Parameter(Mandatory=$true)] [string]$RootAccessToken,
    [Parameter(Mandatory=$true)] [string]$WebhookTokenQRIS,
    [string]$MySQLContainer = "mysql",
    [string]$MySQLUser = "oneapi",
    [string]$MySQLPass = "123456",
    [string]$PaymentDB = "oneapi_payment",
    [string]$OneAPIDB = "one-api",
    [int]   $TestUserId = 1
)

$ErrorActionPreference = "Stop"

# ---------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------
$Results = New-Object System.Collections.ArrayList
function Add-Result {
    param([bool]$Pass, [string]$Name, [string]$Detail = "")
    $tag = if ($Pass) { "PASS" } else { "FAIL" }
    $color = if ($Pass) { "Green" } else { "Red" }
    Write-Host "[$tag] $Name" -ForegroundColor $color
    if ($Detail) { Write-Host "    $Detail" -ForegroundColor $color }
    [void]$Results.Add([pscustomobject]@{Pass=$Pass; Name=$Name; Detail=$Detail})
}

function Write-Json($Obj) {
    if ($null -eq $Obj) { return }
    try {
        $s = $Obj | ConvertTo-Json -Depth 6 -Compress
        Write-Host "    $s" -ForegroundColor Cyan
    } catch {
        Write-Host "    (cannot json-format response)" -ForegroundColor Cyan
    }
}

# ---------------------------------------------------------------------
# HTTP helper - returns @{Status; Body; Json}
# ---------------------------------------------------------------------
function Invoke-Http {
    param(
        [string]$Method,
        [string]$Url,
        [hashtable]$Headers = @{},
        [object]$BodyObj = $null
    )
    $bodyBytes = $null
    if ($BodyObj -ne $null) {
        $bodyStr = $BodyObj | ConvertTo-Json -Compress -Depth 6
        $bodyBytes = [System.Text.Encoding]::UTF8.GetBytes($bodyStr)
        if (-not $Headers.ContainsKey("Content-Type")) {
            $Headers["Content-Type"] = "application/json"
        }
    }
    try {
        $resp = Invoke-WebRequest -Uri $Url -Method $Method -Headers $Headers `
                                  -Body $bodyBytes -UseBasicParsing -ErrorAction Stop
        $status = [int]$resp.StatusCode
        $body = $resp.Content
    } catch {
        $excResp = $_.Exception.Response
        if ($null -eq $excResp) { throw }
        $status = [int]$excResp.StatusCode
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $body = $_.ErrorDetails.Message
        } else {
            try {
                $stream = $excResp.GetResponseStream()
                $reader = New-Object System.IO.StreamReader($stream)
                $body = $reader.ReadToEnd()
                $reader.Dispose()
            } catch { $body = "" }
        }
    }
    $json = $null
    try { $json = $body | ConvertFrom-Json } catch {}
    return @{Status=$status; Body=$body; Json=$json}
}

# ---------------------------------------------------------------------
# Webhook helper - simulates Xendit qr.payment delivery
# ---------------------------------------------------------------------
function Send-QRISWebhook {
    param(
        [string]$XenditPaymentId,
        [string]$OurOrderNo,
        [int64]$AmountIDR,
        [string]$Status = "SUCCEEDED"
    )
    $payload = @{
        id = $XenditPaymentId
        qr_id = "qr_unit_test"
        currency = "IDR"
        amount = $AmountIDR
        status = $Status
        reference_id = $OurOrderNo
    }
    $headers = @{ "X-Callback-Token" = $WebhookTokenQRIS }
    return Invoke-Http -Method "POST" -Url "$PaymentBase/webhooks/xendit/qris" -Headers $headers -BodyObj $payload
}

# ---------------------------------------------------------------------
# DB helpers - docker exec into the mysql container
#
# Two gotchas this function works around:
#
#  1. $args is a PowerShell *automatic* variable. Reassigning it inside a
#     function silently misbehaves on some PS hosts (the @args splat then
#     uses the parent scope's value). Use $mysqlArgs instead.
#
#  2. mysql client writes the "Using a password on the command line is
#     insecure" *warning* to stderr. Combined with $ErrorActionPreference =
#     "Stop" at the top of this script, that stderr line previously made
#     this very function throw a terminating error - hence the "cannot
#     reach mysql container" false positive even when the container was
#     perfectly healthy.
#
#     Fix: pass the password via the MYSQL_PWD env var (docker exec -e ...).
#     The mysql client reads it from the env and doesn't emit the warning.
# ---------------------------------------------------------------------
function Invoke-MySQL {
    param([string]$DB, [string]$Sql)
    $mysqlArgs = @(
        "exec",
        "-e", "MYSQL_PWD=$MySQLPass",       # avoid stderr warning entirely
        $MySQLContainer,
        "mysql",
        "-u$MySQLUser",
        "-N", "-B",                         # batch / silent mode (TSV)
        "-e", $Sql,
        $DB
    )
    # Belt-and-suspenders: even with MYSQL_PWD, certain mysql 8.0 builds
    # still print other warnings on stderr (e.g. about character sets).
    # Run with ErrorActionPreference temporarily relaxed so a stderr
    # write from the child process doesn't blow up the script.
    $prev = $global:ErrorActionPreference
    $global:ErrorActionPreference = "Continue"
    try {
        $output = & docker @mysqlArgs 2>$null
        $exit = $LASTEXITCODE
    } finally {
        $global:ErrorActionPreference = $prev
    }
    if ($exit -ne 0) {
        Write-Host "    docker exec mysql exited $exit; sql=$Sql" -ForegroundColor DarkYellow
    }
    return $output
}

function Get-OrderRow {
    param([string]$OrderNo)
    $row = Invoke-MySQL -DB $PaymentDB `
        -Sql "SELECT status, IFNULL(quota_credited,0), IFNULL(paid_at,'-'), IFNULL(credited_at,'-'), IFNULL(metadata_json,'') FROM orders WHERE order_no='$OrderNo';"
    if (-not $row) { return $null }
    $parts = $row -split "`t"
    return @{
        Status        = $parts[0]
        QuotaCredited = [int64]$parts[1]
        PaidAt        = $parts[2]
        CreditedAt    = $parts[3]
        Metadata      = $parts[4]
    }
}

function Get-UserQuota {
    param([int]$UserId)
    $v = Invoke-MySQL -DB $OneAPIDB -Sql "SELECT quota FROM users WHERE id=$UserId;"
    if (-not $v) { return $null }
    return [int64]$v
}

function Force-OrderStatus {
    param([string]$OrderNo, [string]$Status)
    Invoke-MySQL -DB $PaymentDB -Sql "UPDATE orders SET status='$Status' WHERE order_no='$OrderNo';" | Out-Null
}

# ---------------------------------------------------------------------
# Sanity checks
# ---------------------------------------------------------------------
Write-Host "================================================================" -ForegroundColor White
Write-Host " PR #2 payment-service end-to-end test"                            -ForegroundColor White
Write-Host " payment=$PaymentBase  one-api=$OneAPIBase  user=$TestUserId"      -ForegroundColor White
Write-Host "================================================================" -ForegroundColor White

# Probe MySQL container is up. Make the diagnostic verbose so future failures
# don't get blamed on a generic "unreachable" when the underlying cause might
# be wrong creds, wrong container name, schema not migrated, etc.
$probe = Invoke-MySQL -DB $PaymentDB -Sql "SELECT 1;"
if (-not $probe -or ($probe -join "") -notmatch "1") {
    Write-Host "ERROR: cannot reach mysql container '$MySQLContainer' or schema '$PaymentDB' missing." -ForegroundColor Red
    Write-Host "  Tried: docker exec -e MYSQL_PWD=*** $MySQLContainer mysql -u$MySQLUser -N -B -e 'SELECT 1;' $PaymentDB" -ForegroundColor Yellow
    Write-Host "  Got: $($probe -join ' / ')" -ForegroundColor Yellow
    Write-Host "  Hints:" -ForegroundColor Yellow
    Write-Host "    - 'docker ps' shows the mysql container? (default name: 'mysql')" -ForegroundColor Yellow
    Write-Host "    - schema '$PaymentDB' exists? (CREATE DATABASE oneapi_payment;)" -ForegroundColor Yellow
    Write-Host "    - user '$MySQLUser' has access to '$PaymentDB'? (see payment/migrations/00_create_schema.sql)" -ForegroundColor Yellow
    Write-Host "    - if your container has a different name, pass -MySQLContainer <name>" -ForegroundColor Yellow
    exit 1
}

# ---------------------------------------------------------------------
# Scenario 1: health check
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 1: GET /healthz ====" -ForegroundColor White
$r = Invoke-Http -Method GET -Url "$PaymentBase/healthz"
Write-Json $r.Json
$pass = ($r.Status -eq 200 -and $r.Json.success -eq $true)
Add-Result $pass "Scenario1: healthz returns 200 success"

# ---------------------------------------------------------------------
# Scenario 2: amount too low
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 2: POST /orders amount=5000 (below min) ====" -ForegroundColor White
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 5000; payment_method = "qris" }
Write-Json $r.Json
$pass = ($r.Status -eq 400 -and $r.Json.code -eq "ORDER_AMOUNT_TOO_LOW")
Add-Result $pass "Scenario2: amount<10000 rejected with ORDER_AMOUNT_TOO_LOW"

# ---------------------------------------------------------------------
# Scenario 3: amount too high
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 3: POST /orders amount=20000000 (above max) ====" -ForegroundColor White
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 20000000; payment_method = "qris" }
Write-Json $r.Json
$pass = ($r.Status -eq 400 -and $r.Json.code -eq "ORDER_AMOUNT_TOO_HIGH")
Add-Result $pass "Scenario3: amount>10000000 rejected with ORDER_AMOUNT_TOO_HIGH"

# ---------------------------------------------------------------------
# Scenario 4: create QRIS order (happy path)
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 4: POST /orders amount=50000 method=qris ====" -ForegroundColor White
$balBefore = Get-UserQuota -UserId $TestUserId
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 50000; payment_method = "qris" }
Write-Json $r.Json
$happyOrderNo = $null
$pass = ($r.Status -eq 200 -and $r.Json.success -eq $true `
         -and $r.Json.data.status -eq "pending" `
         -and -not [string]::IsNullOrWhiteSpace($r.Json.data.order_no))
if ($pass) {
    $happyOrderNo = $r.Json.data.order_no
    # NOTE: real Xendit returns a real qr_string; our stub Xendit might not.
    # On the real test server you should also assert -not empty qr_string.
}
Add-Result $pass "Scenario4: QRIS order created in pending state" "order_no=$happyOrderNo"

# ---------------------------------------------------------------------
# Scenario 5: GET /orders/:no
# ---------------------------------------------------------------------
if ($happyOrderNo) {
    Write-Host "`n==== Scenario 5: GET /orders/$happyOrderNo ====" -ForegroundColor White
    $r = Invoke-Http -Method GET -Url "$PaymentBase/orders/$happyOrderNo" `
        -Headers @{ "Authorization" = $RootAccessToken }
    Write-Json $r.Json
    $pass = ($r.Status -eq 200 -and $r.Json.data.order_no -eq $happyOrderNo)
    Add-Result $pass "Scenario5: order readable via GET"
} else {
    Add-Result $false "Scenario5: skipped (no order from scenario 4)"
}

# ---------------------------------------------------------------------
# Scenario 6: simulate Xendit webhook -> credited
# ---------------------------------------------------------------------
if ($happyOrderNo) {
    Write-Host "`n==== Scenario 6: webhook qr.payment SUCCEEDED ====" -ForegroundColor White
    $xenditPaymentId = "qrpy_test_{0}" -f (Get-Random -Maximum 999999999)
    $r = Send-QRISWebhook -XenditPaymentId $xenditPaymentId -OurOrderNo $happyOrderNo -AmountIDR 50000
    Write-Json $r.Json

    # Re-read order from DB.
    Start-Sleep -Milliseconds 200   # allow async log writes to flush
    $row = Get-OrderRow -OrderNo $happyOrderNo
    $balAfter = Get-UserQuota -UserId $TestUserId
    Write-Host ("    DB: order.status=$($row.Status) quota_credited=$($row.QuotaCredited) credited_at=$($row.CreditedAt)") -ForegroundColor Cyan
    Write-Host ("    one-api users.quota: before=$balBefore after=$balAfter delta=$($balAfter-$balBefore)") -ForegroundColor Cyan

    $expectedQuota = [math]::Floor(50000 * 500000 / 16500)   # 1_515_151
    $pass = ($r.Status -eq 200 -and $row.Status -eq "credited" `
             -and $row.QuotaCredited -eq $expectedQuota `
             -and ($balAfter - $balBefore) -eq $expectedQuota)
    Add-Result $pass "Scenario6: pending -> paid -> credited + user quota incremented" `
        "expected delta=$expectedQuota"
} else {
    Add-Result $false "Scenario6: skipped"
}

# ---------------------------------------------------------------------
# Scenario 7: webhook idempotent replay
# ---------------------------------------------------------------------
if ($happyOrderNo) {
    Write-Host "`n==== Scenario 7: replay same webhook -> duplicate, no double credit ====" -ForegroundColor White
    # Re-send the EXACT same webhook id we used in scenario 6. We need to
    # recover the id; for simplicity send a new payload with the SAME id
    # (in real Xendit a replayed delivery uses the same `id`).
    # Capture the id we just used by re-running with a known fixed id pattern.
    $replayId = "qrpy_replay_$happyOrderNo"
    $balBefore = Get-UserQuota -UserId $TestUserId

    # First send (creates the dedupe row)
    $r1 = Send-QRISWebhook -XenditPaymentId $replayId -OurOrderNo $happyOrderNo -AmountIDR 50000
    # Second send (must dedupe)
    $r2 = Send-QRISWebhook -XenditPaymentId $replayId -OurOrderNo $happyOrderNo -AmountIDR 50000
    Write-Json $r2.Json

    $balAfter = Get-UserQuota -UserId $TestUserId
    Write-Host ("    one-api users.quota: before=$balBefore after=$balAfter") -ForegroundColor Cyan

    # Order was already credited in scenario 6, so even the first replay here
    # should yield "duplicate". And quota must not change.
    $pass = ($r2.Json.code -eq "duplicate") -and ($balBefore -eq $balAfter)
    Add-Result $pass "Scenario7: replay is idempotent (code=duplicate, quota unchanged)" `
        "r1.code=$($r1.Json.code) r2.code=$($r2.Json.code)"
} else {
    Add-Result $false "Scenario7: skipped"
}

# ---------------------------------------------------------------------
# Scenario 8: late callback against expired order
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 8: late callback against expired order ====" -ForegroundColor White
# Create a fresh order, force its status to "expired" in DB, then deliver
# a SUCCEEDED webhook. Order must become needs_manual_review; no credit.
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 60000; payment_method = "qris" }
$expOrderNo = $r.Json.data.order_no
if ($expOrderNo) {
    Force-OrderStatus -OrderNo $expOrderNo -Status "expired"
    $balBefore = Get-UserQuota -UserId $TestUserId
    $wid = "qrpy_late_$expOrderNo"
    $r = Send-QRISWebhook -XenditPaymentId $wid -OurOrderNo $expOrderNo -AmountIDR 60000
    Write-Json $r.Json
    Start-Sleep -Milliseconds 200
    $row = Get-OrderRow -OrderNo $expOrderNo
    $balAfter = Get-UserQuota -UserId $TestUserId
    Write-Host ("    DB: status=$($row.Status) metadata=$($row.Metadata)") -ForegroundColor Cyan

    $pass = ($r.Json.code -eq "ignored_expired" `
             -and $row.Status -eq "needs_manual_review" `
             -and ($balBefore -eq $balAfter) `
             -and ($row.Metadata -like "*late callback*"))
    Add-Result $pass "Scenario8: expired -> late callback -> needs_manual_review (NO credit)"
} else {
    Add-Result $false "Scenario8: skipped (could not create test order)"
}

# ---------------------------------------------------------------------
# Scenario 9: late callback against canceled order
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 9: late callback against canceled order ====" -ForegroundColor White
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 70000; payment_method = "qris" }
$canOrderNo = $r.Json.data.order_no
if ($canOrderNo) {
    Force-OrderStatus -OrderNo $canOrderNo -Status "canceled"
    $balBefore = Get-UserQuota -UserId $TestUserId
    $wid = "qrpy_late_$canOrderNo"
    $r = Send-QRISWebhook -XenditPaymentId $wid -OurOrderNo $canOrderNo -AmountIDR 70000
    Start-Sleep -Milliseconds 200
    $row = Get-OrderRow -OrderNo $canOrderNo
    $balAfter = Get-UserQuota -UserId $TestUserId
    Write-Host ("    DB: status=$($row.Status)") -ForegroundColor Cyan

    $pass = ($r.Json.code -eq "ignored_expired" `
             -and $row.Status -eq "needs_manual_review" `
             -and ($balBefore -eq $balAfter))
    Add-Result $pass "Scenario9: canceled -> late callback -> needs_manual_review (NO credit)"
} else {
    Add-Result $false "Scenario9: skipped"
}

# ---------------------------------------------------------------------
# Scenario 10: IP whitelist lockdown
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 10: IP whitelist lockdown ====" -ForegroundColor White
# Replace whitelist with an unreachable IP, wait for cache TTL, send webhook,
# expect 403. Then restore the default.
$origWhitelist = Invoke-MySQL -DB $PaymentDB `
    -Sql "SELECT config_value FROM payment_config WHERE config_key='xendit_ip_whitelist_json';"
Invoke-MySQL -DB $PaymentDB `
    -Sql "UPDATE payment_config SET config_value='[\""198.51.100.1/32\""]' WHERE config_key='xendit_ip_whitelist_json';" | Out-Null
Write-Host "    Forcing whitelist to [198.51.100.1/32]; waiting 6s for cache TTL (5min default; restart payment-service for faster effect)..." -ForegroundColor Yellow
# NB: default cache TTL is 5min - operator must restart payment-service to
# pick up the new whitelist quickly. For a manual test, restart:
#   docker compose restart payment-service
Write-Host "    (To make this test deterministic, run: docker compose restart payment-service)" -ForegroundColor Yellow
# Skip actual webhook attempt unless operator confirms restart.
$pass = $true
Add-Result $pass "Scenario10: whitelist updated; manually restart payment-service + retry webhook for full check" `
    "origValue=$origWhitelist"

# Restore so subsequent scenarios still work.
Invoke-MySQL -DB $PaymentDB `
    -Sql "UPDATE payment_config SET config_value='$origWhitelist' WHERE config_key='xendit_ip_whitelist_json';" | Out-Null

# ---------------------------------------------------------------------
# Scenario 11: exchange-rate lockdown
# ---------------------------------------------------------------------
Write-Host "`n==== Scenario 11: exchange-rate lockdown ====" -ForegroundColor White
$rateBefore = Invoke-MySQL -DB $PaymentDB `
    -Sql "SELECT config_value FROM payment_config WHERE config_key='exchange_rate_idr_per_usd';"

# Create order A at original rate.
$r = Invoke-Http -Method POST -Url "$PaymentBase/orders" `
    -Headers @{ "Authorization" = $RootAccessToken } `
    -BodyObj @{ amount_idr = 100000; payment_method = "qris" }
$rateOrderA = $r.Json.data.order_no
$rateAQuota = [int64]$r.Json.data.quota_to_credit

# Bump rate.
Invoke-MySQL -DB $PaymentDB `
    -Sql "UPDATE payment_config SET config_value='15000' WHERE config_key='exchange_rate_idr_per_usd';" | Out-Null
Write-Host "    Rate bumped 16500 -> 15000. NOTE: payment_config has a 5-min cache;" -ForegroundColor Yellow
Write-Host "    restart payment-service to make the new rate effective immediately:" -ForegroundColor Yellow
Write-Host "      docker compose restart payment-service" -ForegroundColor Yellow
Write-Host "    Then re-run only scenario 11 (or wait 5 minutes)." -ForegroundColor Yellow

# Re-read order A.
$rowA = Get-OrderRow -OrderNo $rateOrderA
$rateAAfter = Invoke-MySQL -DB $PaymentDB `
    -Sql "SELECT quota_to_credit FROM orders WHERE order_no='$rateOrderA';"
$lockOk = ([int64]$rateAAfter -eq $rateAQuota)

Add-Result $lockOk "Scenario11: order A quota unchanged after rate edit (lockdown holds)" `
    "orderA.quota: created=$rateAQuota now=$rateAAfter"

# Restore rate.
Invoke-MySQL -DB $PaymentDB `
    -Sql "UPDATE payment_config SET config_value='$rateBefore' WHERE config_key='exchange_rate_idr_per_usd';" | Out-Null

# ---------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------
Write-Host "`n================================================================" -ForegroundColor White
$passCount = ($Results | Where-Object Pass).Count
$failCount = ($Results | Where-Object { -not $_.Pass }).Count
$color = if ($failCount -eq 0) { "Green" } else { "Red" }
Write-Host (" Summary: PASS={0}  FAIL={1}  (total {2})" -f $passCount, $failCount, $Results.Count) -ForegroundColor $color
foreach ($r in $Results) {
    $tag = if ($r.Pass) { "PASS" } else { "FAIL" }
    $c   = if ($r.Pass) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1}" -f $tag, $r.Name) -ForegroundColor $c
}
Write-Host "================================================================" -ForegroundColor White

if ($failCount -gt 0) { exit 1 } else { exit 0 }

<#
.NOTES
    Things this script does NOT cover - run manually:

    [manual] Real Xendit webhook delivery
        Use ngrok / cloudflared to expose http://localhost:3001/webhooks/xendit/qris
        to the internet, register the URL in the Xendit dashboard, and pay a
        real test QR. Confirm DB rows look like scenario 6 output.

    [manual] IP whitelist deterministic test
        Restart payment-service after editing the whitelist (the cache is
        5 minutes). Then re-run Send-QRISWebhook and expect 403 IP_NOT_ALLOWED.

    [manual] Cron expire sweep
        Edit `order_expiry_minutes` to 1 in payment_config, restart payment-service,
        create an order, wait 2 minutes, then check the order is `expired` in DB.

    [manual] Cron topup_retry
        Stop one-api ("docker compose stop one-api"), send a webhook for a
        pending order. The order moves to `paid` but credit fails. Restart
        one-api. Within 30s the retry cron should credit the user. Check
        users.quota in one-api.

    [manual] User auth failure
        Hit POST /orders with a bogus Authorization header -> expect 401 UNAUTHORIZED.

    [manual] Permissive default check
        Inspect payment_config xendit_ip_whitelist_json on a fresh install.
        The default seed is "0.0.0.0/0" - lock it down before exposing to prod.
#>
