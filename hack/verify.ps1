# hack/verify.ps1 -- Windows verification entry point (same checks as `make verify`).
#
# WHY THIS FILE IS ASCII-ONLY:
#   Windows PowerShell 5.1 reads a .ps1 file WITHOUT a BOM as ANSI (the OEM code page),
#   not UTF-8. Any non-ASCII literal here would be corrupted at **parse** time, and the
#   failure mode is nasty: the script exits with -1 and prints nothing at all, which
#   looks like "PowerShell is broken" rather than "the file encoding is wrong".
#   Adding a BOM fixes it for today's file, but the next person editing it with a
#   BOM-unaware editor silently reintroduces the bug. Pure ASCII removes the dependency
#   on file encoding entirely. Project docs are in Chinese; this script intentionally is not.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File hack/verify.ps1
#   powershell -ExecutionPolicy Bypass -File hack/verify.ps1 -Task test
#
# Toolchain resolution: $env:LOCALAPPDATA\sandbox-tools first, then whatever is on PATH.

param(
    [ValidateSet('verify', 'fmt', 'fmt-check', 'vet', 'build', 'test', 'cover', 'manifests', 'generate', 'cleanup')]
    [string]$Task = 'verify'
)

$ErrorActionPreference = 'Stop'

# ---- Resolve toolchain ----
$toolRoot = Join-Path $env:LOCALAPPDATA 'sandbox-tools'
if (Test-Path (Join-Path $toolRoot 'go\bin\go.exe')) {
    $env:Path = "$toolRoot\go\bin;$toolRoot\bin;$env:Path"
    $env:GOBIN = Join-Path $toolRoot 'bin'
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host 'ERROR: go not found. Install Go 1.27+ or add go.exe to PATH.' -ForegroundColor Red
    exit 127
}

# gofmt takes directory arguments, not globs.
$srcDirs = @('./api', './cmd', './internal')
$failed = @()

function Invoke-Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host ''
    Write-Host "=== $Name ===" -ForegroundColor Cyan
    # Reset before running: $LASTEXITCODE is process-wide and would otherwise leak
    # the previous step's result into this one (a real footgun in this script).
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        Write-Host "$Name FAILED (exit=$LASTEXITCODE)" -ForegroundColor Red
        $script:failed += $Name
    } else {
        Write-Host "$Name OK" -ForegroundColor Green
    }
}

function Invoke-Fmt { gofmt -w $srcDirs }

function Invoke-FmtCheck {
    $out = gofmt -l $srcDirs
    if ($out) {
        Write-Host 'The following files are not gofmt-clean:' -ForegroundColor Red
        $out | ForEach-Object { Write-Host "  $_" }
        $global:LASTEXITCODE = 1
    } else {
        $global:LASTEXITCODE = 0
    }
}

function Invoke-Manifests {
    # Use an explicit package path rather than ./api/...
    # PowerShell consumes the `...` token itself, so controller-gen receives a truncated
    # path and reports "no Go files in ...\api" -- a confusing failure for a one-character
    # difference. Explicit paths sidestep the whole class of problem.
    controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
}

function Invoke-Generate {
    controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1
}

function Clear-StrayEnvtest {
    # envtest starts one etcd + one kube-apiserver per suite. When a test binary is
    # killed instead of exiting cleanly (Defender file locks on *.test.exe,
    # Ctrl+C, a timeout), those children are NOT reaped and they accumulate
    # silently: one working session on this machine reached 36 etcd + 34
    # kube-apiserver, about 6 GB of RAM, which then looked like "the machine is
    # out of memory" rather than "a test leaked processes".
    #
    # The filter matters: only processes whose executable lives under the
    # project's envtest asset directory are touched, so a real local cluster
    # (kind / kubeadm / a colleague's control plane) can never be hit.
    $stale = Get-Process etcd, kube-apiserver -ErrorAction SilentlyContinue |
        Where-Object { try { $_.Path -like '*sandbox-tools\envtest*' } catch { $false } }
    if (-not $stale) {
        Write-Host 'no stray envtest processes.'
        return
    }
    $mb = [int]((($stale | Measure-Object -Property WorkingSet64 -Sum).Sum) / 1MB)
    Write-Host "stopping $($stale.Count) stray envtest processes (~$mb MB)"
    $stale | Stop-Process -Force -ErrorAction SilentlyContinue
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
    'cleanup'     { Invoke-Step 'cleanup' { Clear-StrayEnvtest } }
    'verify' {
        Invoke-Step 'fmt-check' { Invoke-FmtCheck }
        Invoke-Step 'vet'       { go vet ./... }
        Invoke-Step 'build'     { go build ./... }
        Invoke-Step 'test'      { go test ./... -count=1 }
        # Always last: a clean run leaves nothing behind, but a run that was
        # interrupted must not leave 6 GB of control planes running.
        Invoke-Step 'cleanup'   { Clear-StrayEnvtest }
    }
}

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host "FAILED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host 'All checks passed.' -ForegroundColor Green
