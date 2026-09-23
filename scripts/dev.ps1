# =============================================================================
# 一键启动开发环境（后端 API + 前端控制台）
#   用法: pwsh -File scripts/dev.ps1            # 两个进程前台运行
#         pwsh -File scripts/dev.ps1 -SkipFrontend
# =============================================================================
param(
    [switch]$SkipFrontend,
    [switch]$SkipInstall
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$backend = Join-Path $root 'backend'
$frontend = Join-Path $root 'frontend'

Write-Host '==> 准备后端依赖' -ForegroundColor Cyan
Push-Location $backend
go mod tidy
Pop-Location

if (-not $SkipInstall) {
    if (-not (Test-Path (Join-Path $frontend 'node_modules'))) {
        Write-Host '==> 安装前端依赖（首次较慢）' -ForegroundColor Cyan
        Push-Location $frontend
        pnpm install
        Pop-Location
    }
}

Write-Host '==> 启动后端 :8080' -ForegroundColor Green
$backendProc = Start-Process -FilePath 'go' `
    -ArgumentList 'run', './cmd/server', '--config', (Join-Path $root 'deploy/config.example.json') `
    -WorkingDirectory $backend -PassThru

if ($SkipFrontend) {
    Write-Host "后端 PID=$($backendProc.Id)，Ctrl+C 结束" -ForegroundColor Yellow
    Wait-Process -Id $backendProc.Id
    exit 0
}

Write-Host '==> 启动前端 :5173' -ForegroundColor Green
$frontendProc = Start-Process -FilePath 'pnpm' -ArgumentList 'run', 'dev' -WorkingDirectory $frontend -PassThru

Write-Host ''
Write-Host '控制台: http://127.0.0.1:5173    API: http://127.0.0.1:8080/api/v1' -ForegroundColor Green
Write-Host '默认账号: 租户 demo / 用户 admin / 密码 admin123' -ForegroundColor Green
Write-Host 'Ctrl+C 结束两个进程' -ForegroundColor Yellow
Write-Host ''

try {
    Wait-Process -Id $backendProc.Id
}
finally {
    if ($frontendProc -and -not $frontendProc.HasExited) {
        Stop-Process -Id $frontendProc.Id -Force -ErrorAction SilentlyContinue
    }
}
