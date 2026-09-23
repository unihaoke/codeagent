# =============================================================================
# 端到端冒烟脚本：验证「单仓库精准修复」与「多仓库分组联合排查」两条主链路
#
# 前置：
#   1) 后端已启动，且控制台已完成初始化：
#      - 「AI 设置」中至少配置一个可用模型（否则任务会因无模型而失败）；
#      - 「仓库」中至少录入一个真实可达的仓库；需要验证分组场景时再建一个分组。
#   2) 本脚本不再依赖任何演示数据：缺少仓库/分组时会跳过对应场景。
#
# 用法：
#   powershell -File scripts/smoke.ps1                       # 自动登录取 JWT
#   powershell -File scripts/smoke.ps1 -ApiKey ca_live_xxx
#   powershell -File scripts/smoke.ps1 -BaseUrl http://127.0.0.1:8080 -RepoKey order-service
# =============================================================================
param(
    [string]$BaseUrl = 'http://127.0.0.1:8080',
    [string]$ApiKey = $env:CA_KEY,
    [string]$TenantKey = 'demo',
    [string]$Username = 'admin',
    [string]$Password = 'admin123',
    # 指定参与验证的仓库 Key 与分组 Key；留空则取列表中的第一个。
    [string]$RepoKey = '',
    [string]$GroupKey = '',
    # 租户隔离验证用的第二个租户；不存在则跳过该步。
    [string]$SecondTenantKey = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Write-Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Ok($msg) { Write-Host "    OK  $msg" -ForegroundColor Green }
function Write-Bad($msg) { Write-Host "    ERR $msg" -ForegroundColor Red }

# 运行详情接口返回 { run: {...}, resolution, evidence, ... } 包装，本函数统一取出内部 run 对象。
function Unwrap-Run($detail) {
    if ($null -eq $detail) { return $null }
    if (($detail.PSObject.Properties.Name -contains 'run') -and ($null -ne $detail.run)) { return $detail.run }
    return $detail
}

# 报告详情接口返回 { report: {...} } 包装。
function Unwrap-Report($detail) {
    if ($null -eq $detail) { return $null }
    if (($detail.PSObject.Properties.Name -contains 'report') -and ($null -ne $detail.report)) { return $detail.report }
    return $detail
}

# --- 0. 健康检查 -------------------------------------------------------------
Write-Step "健康检查 $BaseUrl/healthz"
try {
    $health = Invoke-RestMethod -Uri "$BaseUrl/healthz" -Method Get -TimeoutSec 10
    $h = if ($null -ne $health.data) { $health.data } else { $health }
    Write-Ok "status=$($h.status) version=$($h.version)"
}
catch {
    Write-Bad "后端不可达：$($_.Exception.Message)"
    Write-Host "请先启动后端：cd backend; go run ./cmd/server --config ../deploy/config.example.json" -ForegroundColor Yellow
    exit 1
}

# --- 1. 认证：无密钥时用账号登录换取 JWT --------------------------------------
$headers = @{}
if ($ApiKey) {
    $headers['X-API-Key'] = $ApiKey
    Write-Ok "使用 API Key 认证（前缀 $($ApiKey.Substring(0, [Math]::Min(12, $ApiKey.Length)))…）"
}
else {
    Write-Step "使用控制台账号登录（租户 $TenantKey / 用户 $Username）"
    $loginBody = @{ tenantKey = $TenantKey; username = $Username; password = $Password } | ConvertTo-Json
    try {
        $login = Invoke-RestMethod -Uri "$BaseUrl/api/v1/auth/login" -Method Post -Body $loginBody -ContentType 'application/json' -TimeoutSec 15
        $headers['Authorization'] = "Bearer $($login.data.token)"
        Write-Ok "登录成功，租户=$($login.data.subject.tenantId)"
    }
    catch {
        Write-Bad "登录失败：$($_.Exception.Message)"
        exit 1
    }
}

function Invoke-Api {
    param([string]$Method, [string]$Path, $Body)
    $params = @{ Uri = "$BaseUrl/api/v1$Path"; Method = $Method; Headers = $headers; TimeoutSec = 60 }
    if ($Body) {
        $params['Body'] = ($Body | ConvertTo-Json -Depth 12)
        $params['ContentType'] = 'application/json'
    }
    return Invoke-RestMethod @params
}

# --- 2. 查询仓库与分组索引 ----------------------------------------------------
Write-Step '查询仓库与分组索引'
$repos = (Invoke-Api -Method Get -Path '/repos?pageSize=200').data.items
$groups = (Invoke-Api -Method Get -Path '/groups?pageSize=100').data.items
Write-Ok "仓库 $($repos.Count) 个，分组 $($groups.Count) 个"

# 参与验证的目标：优先命令行指定，其次列表首项（不再假设固定的演示数据）。
$orderRepo = if ($RepoKey) {
    $repos | Where-Object { $_.key -eq $RepoKey } | Select-Object -First 1
}
else { $repos | Select-Object -First 1 }

$coreGroup = if ($GroupKey) {
    $g = $groups | Where-Object { $_.group.key -eq $GroupKey } | Select-Object -First 1
    if ($g) { $g.group } else { $null }
}
else {
    $g = $groups | Select-Object -First 1
    if ($g) { $g.group } else { $null }
}
if ($orderRepo) { Write-Ok "目标仓库 $($orderRepo.key)" } else { Write-Host '    提示：没有任何仓库，单仓库场景将被跳过' -ForegroundColor Yellow }
if ($coreGroup) { Write-Ok "目标分组 $($coreGroup.key)" } else { Write-Host '    提示：没有任何分组，分组场景将被跳过' -ForegroundColor Yellow }

$npeStack = @"
2024-06-11 10:23:41.883 ERROR [order-service,8f2c1d9a4b7e6f01] 1 --- [http-nio-8080-exec-3] c.a.o.web.OrderController : 订单详情查询失败

java.lang.NullPointerException: Cannot invoke "com.acme.order.entity.Order.getAmount()" because "order" is null
	at com.acme.order.service.OrderService.toDetail(OrderService.java:88)
	at com.acme.order.service.OrderService.queryDetail(OrderService.java:64)
	at com.acme.order.web.OrderController.detail(OrderController.java:41)
Caused by: com.acme.common.exception.RemoteCallException: inventory-service 调用失败
	at com.acme.order.client.InventoryClient.query(InventoryClient.java:52)
	... 23 more
"@

function Wait-Run {
    param([string]$RunId, [int]$TimeoutSec = 180)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $run = Unwrap-Run ((Invoke-Api -Method Get -Path "/runs/$RunId").data)
        if (@('succeeded', 'needs_review', 'failed', 'cancelled', 'degraded') -contains $run.state) { return $run }
        Start-Sleep -Milliseconds 900
    }
    throw "任务 $RunId 在 $TimeoutSec 秒内未进入终态"
}

# --- 3. 单仓库精准修复 --------------------------------------------------------
Write-Step '场景一：单仓库精准修复'
if (-not $orderRepo) {
    Write-Host '    跳过：请先在控制台「仓库」中录入至少一个仓库' -ForegroundColor Yellow
}
else {
    $body = @{
        mode        = 'single_repo'
        repoId      = $orderRepo.id
        source      = 'manual'
        title       = '[冒烟] 订单详情接口 NPE'
        environment = 'prod'
        stacktrace  = $npeStack
        logs        = "2024-06-11 10:23:41.812 WARN [order-service] InventoryClient host=inventory-svc endpoint=/api/inventory/query"
    }

    $run = (Invoke-Api -Method Post -Path '/tasks' -Body $body).data
    Write-Ok "任务已受理 runId=$($run.id) state=$($run.state)"

    $final = Wait-Run -RunId $run.id
    Write-Ok "终态 state=$($final.state) severity=$($final.severity) 耗时=$($final.elapsedMs)ms"
    Write-Host "    锁定仓库: $((($final.resolution | ForEach-Object { "$($_.repoKey)@$($_.commit.Substring(0,[Math]::Min(8,$_.commit.Length)))" }) -join ', '))"
    Write-Host "    根因: $($final.rootCause.summary)"
    Write-Host "    根因分类: $($final.rootCause.category) 置信度=$($final.rootCause.confidence)"
    Write-Host "    加载文件数: $($final.usage.filesLoaded) 代码字符: $($final.usage.codeChars)"
    Write-Host "    技能调用: $($final.usage.skillCalls) 模型调用: $($final.usage.modelCalls) Token: $($final.usage.totalTokens)"
    Write-Host "    增量补丁: $($final.patches.Count) 个"
    foreach ($p in $final.patches) {
        Write-Host "      - $($p.repoKey)/$($p.filePath) 风险=$($p.risk) 状态=$($p.status)"
    }
    if ($final.verification) {
        Write-Host "    沙箱验证: passed=$($final.verification.passed) apply=$($final.verification.applyResult) 降级=$($final.verification.degraded)"
        foreach ($c in $final.verification.checks) {
            Write-Host "      - $($c.name) passed=$($c.passed) skipped=$($c.skipped) $($c.skipReason)"
        }
    }
    if ($final.warnings) { Write-Host "    警告: $($final.warnings -join ' | ')" -ForegroundColor Yellow }
    if ($final.reportId) {
        $md = (Invoke-Api -Method Get -Path "/reports/$($final.reportId)").data
        Write-Ok "报告已归档 reportId=$($final.reportId)，Markdown 长度=$($md.markdown.Length)"
    }

    # 调用轨迹
    $skillCalls = (Invoke-Api -Method Get -Path "/runs/$($run.id)/skill-calls?pageSize=50").data.items
    Write-Host "    技能调用轨迹: $($skillCalls.Count) 条"
    foreach ($sc in $skillCalls) {
        Write-Host "      - $($sc.skill)@$($sc.version) stage=$($sc.stage) status=$($sc.status) 尝试=$($sc.attempts) 耗时=$($sc.durationMs)ms"
    }
    $modelCalls = (Invoke-Api -Method Get -Path "/runs/$($run.id)/model-calls?pageSize=50").data.items
    Write-Host "    模型推理轨迹: $($modelCalls.Count) 条"
    foreach ($mc in $modelCalls) {
        Write-Host "      - $($mc.provider)/$($mc.model) tier=$($mc.tier) stage=$($mc.stage) status=$($mc.status) schemaValid=$($mc.schemaValid) tokens=$($mc.totalTokens)"
    }
}

# --- 4. 多仓库分组联合排查 ----------------------------------------------------
Write-Step '场景二：多仓库分组联合排查'
if (-not $coreGroup) {
    Write-Host '    跳过：请先在控制台「分组」中创建一个含多个仓库的分组' -ForegroundColor Yellow
}
else {
    $body = @{
        mode        = 'group'
        groupId     = $coreGroup.id
        source      = 'alert'
        title       = '[冒烟] 订单链路跨服务故障'
        environment = 'prod'
        stacktrace  = $npeStack
        logs        = "2024-06-11 10:23:41.883 ERROR [order-service] api-gateway -> order-service -> inventory-service 链路异常"
        entryFiles  = @('src/main/java/com/acme/order/service/OrderService.java')
    }
    $run = (Invoke-Api -Method Post -Path '/tasks' -Body $body).data
    Write-Ok "任务已受理 runId=$($run.id) mode=$($run.mode)"

    $final = Wait-Run -RunId $run.id
    Write-Ok "终态 state=$($final.state) 候选/锁定仓库: $((($final.resolution | ForEach-Object { $_.repoKey }) -join ', '))"
    Write-Host "    根因: $($final.rootCause.summary)"
    Write-Host "    补丁: $($final.patches.Count) 个，加载文件: $($final.usage.filesLoaded) 个"
    if ($final.warnings) { Write-Host "    警告: $($final.warnings -join ' | ')" -ForegroundColor Yellow }
    if ($final.reportId) {
        $rep = (Invoke-Api -Method Get -Path "/reports/$($final.reportId)").data
        Write-Ok "报告摘要: $($rep.summary)"
        Write-Host "    跨仓库链路边: $($rep.chainFlow.Count) 条"
        foreach ($e in $rep.chainFlow) {
            Write-Host "      - $($e.fromRepo) -> $($e.toRepo) [$($e.protocol)] $($e.endpoint) 置信度=$($e.confidence) 异常=$($e.anomaly)"
        }
        Write-Host "    建议: $($rep.suggestions -join ' | ')"
    }
}

# --- 5. 幂等验证 --------------------------------------------------------------
Write-Step '幂等验证：同 idempotencyKey 重复提交'
if ($orderRepo) {
    $key = "smoke-$([guid]::NewGuid().ToString('N').Substring(0,8))"
    $body = @{
        mode = 'single_repo'; repoId = $orderRepo.id; source = 'cicd'
        title = '[冒烟] 幂等测试'; stacktrace = $npeStack; idempotencyKey = $key
    }
    $r1 = (Invoke-Api -Method Post -Path '/tasks' -Body $body).data
    $r2 = (Invoke-Api -Method Post -Path '/tasks' -Body $body).data
    if ($r1.id -eq $r2.id) { Write-Ok "两次提交复用同一 run：$($r1.id)" }
    else { Write-Bad "幂等失效：$($r1.id) != $($r2.id)" }
}

# --- 6. 租户隔离验证 ----------------------------------------------------------
# 需要先用控制台创建第二个租户并通过 -SecondTenantKey 指定；未提供则跳过。
Write-Step '租户隔离验证'
if (-not $SecondTenantKey) {
    Write-Host '    跳过：未指定 -SecondTenantKey（可在控制台创建第二个租户后验证跨租户隔离）' -ForegroundColor Yellow
}
else {
    $acmeBody = @{ tenantKey = $SecondTenantKey; username = $Username; password = $Password } | ConvertTo-Json
    try {
        $acmeLogin = Invoke-RestMethod -Uri "$BaseUrl/api/v1/auth/login" -Method Post -Body $acmeBody -ContentType 'application/json' -TimeoutSec 15
        $acmeHeaders = @{ Authorization = "Bearer $($acmeLogin.data.token)" }
        $acmeRepos = Invoke-RestMethod -Uri "$BaseUrl/api/v1/repos?pageSize=50" -Headers $acmeHeaders -TimeoutSec 20
        if ($acmeRepos.data.total -eq 0) {
            Write-Ok "$SecondTenantKey 租户看不到 $TenantKey 租户的任何仓库（跨租户隔离生效）"
        }
        else {
            Write-Bad "租户隔离失效：$SecondTenantKey 看到了 $($acmeRepos.data.total) 个仓库"
        }
    }
    catch {
        Write-Host "    $SecondTenantKey 租户登录或查询失败：$($_.Exception.Message)" -ForegroundColor Yellow
    }
}

# --- 7. 可观测汇总 ------------------------------------------------------------
Write-Step '可观测汇总'
$obs = (Invoke-Api -Method Get -Path '/observability/summary').data.summary
Write-Ok "任务总数=$($obs.tasksTotal) 运行总数=$($obs.runsTotal) 修复率=$([Math]::Round($obs.fixRate*100,1))% 平均耗时=$($obs.avgElapsedMs)ms"
Write-Host "    状态分布: $(($obs.stateDist.PSObject.Properties | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ', ')"
Write-Host "    根因分布: $(($obs.categoryDist.PSObject.Properties | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ', ')"
Write-Host "    技能调用=$($obs.skillCalls) 失败=$($obs.skillFailures) 模型调用=$($obs.modelCalls) Token=$($obs.modelTokens) 缓存命中=$($obs.cacheHits)"

$modelStats = (Invoke-Api -Method Get -Path '/models/stats').data.stats
Write-Host "    模型层: 调用=$($modelStats.totalCalls) 失败=$($modelStats.failedCalls) 兜底=$($modelStats.fallbackCalls) 结构化修复=$($modelStats.repairedOutputs)"

$skills = (Invoke-Api -Method Get -Path '/skills').data.items
Write-Ok "已注册技能 $($skills.Count) 个：$(($skills | ForEach-Object { $_.manifest.name }) -join ', ')"

Write-Host ''
Write-Host '冒烟测试完成。控制台可在 http://127.0.0.1:5173 查看任务详情、增量 Patch 与报告。' -ForegroundColor Green
