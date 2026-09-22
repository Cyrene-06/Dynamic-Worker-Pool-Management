# hack/verify.ps1 —— Windows 上没有 make 时的等价验证入口。
#
# 为什么需要它：Makefile 依赖 POSIX shell，而 Windows 默认没有 make/bash。
# 如果本地唯一能跑验证的方式是"先装 make + bash"，那"验证"这件事就会被跳过 ——
# 实际上"跳过验证"远比"多写一个脚本"昂贵。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File hack/verify.ps1
#   powershell -ExecutionPolicy Bypass -File hack/verify.ps1 -Task test
#
# 工具链解析顺序：$env:LOCALAPPDATA\sandbox-tools → 已在 PATH 中的 go。

param(
    [ValidateSet('verify', 'fmt', 'fmt-check', 'vet', 'build', 'test', 'cover', 'manifests', 'generate')]
    [string]$Task = 'verify'
)

$ErrorActionPreference = 'Stop'

# ---- 解析工具链 ----
$toolRoot = Join-Path $env:LOCALAPPDATA 'sandbox-tools'
if (Test-Path (Join-Path $toolRoot 'go\bin\go.exe')) {
    $env:Path = "$toolRoot\go\bin;$toolRoot\bin;$env:Path"
    $env:GOBIN = Join-Path $toolRoot 'bin'
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "找不到 go。请先安装 Go 1.23+，或把 go.exe 加入 PATH。" -ForegroundColor Red
    exit 127
}

$srcDirs = @('./api', './cmd', './internal')
$failed = @()

function Invoke-Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host "`n=== $Name ===" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) {
        Write-Host "$Name 失败（exit=$LASTEXITCODE）" -ForegroundColor Red
        $script:failed += $Name
    } else {
        Write-Host "$Name 通过" -ForegroundColor Green
    }
}

function Invoke-Fmt { gofmt -w $srcDirs }

function Invoke-FmtCheck {
    $out = gofmt -l $srcDirs
    if ($out) {
        Write-Host "以下文件未通过 gofmt：" -ForegroundColor Red
        $out | ForEach-Object { Write-Host "  $_" }
        $global:LASTEXITCODE = 1
    } else {
        $global:LASTEXITCODE = 0
    }
}

function Invoke-Manifests {
    # paths 用显式包路径而不是 ./api/...：PowerShell 会把 `...` 当作自己的记号，
    # 传给 controller-gen 时会丢掉，结果报 "no Go files in ...\api"。
    controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
}

function Invoke-Generate {
    controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1
}

switch ($Task) {
    'fmt'         { Invoke-Fmt }
    'fmt-check'   { Invoke-Step 'fmt-check' { Invoke-FmtCheck } }
    'vet'         { Invoke-Step 'vet' { go vet ./... } }
    'build'       { Invoke-Step 'build' { go build ./... } }
    'test'        { Invoke-Step 'test' { go test ./... -count=1 } }
    'cover'       { Invoke-Step 'cover' { go test ./... -count=1 -coverprofile=cover.out; go tool cover -func=cover.out | Select-Object -Last 1 } }
    'manifests'   { Invoke-Step 'manifests' { Invoke-Manifests } }
    'generate'    { Invoke-Step 'generate' { Invoke-Generate } }
    'verify' {
        Invoke-Step 'fmt-check' { Invoke-FmtCheck }
        Invoke-Step 'vet'       { go vet ./... }
        Invoke-Step 'build'     { go build ./... }
        Invoke-Step 'test'      { go test ./... -count=1 }
    }
}

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host "失败项: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "全部通过。" -ForegroundColor Green
