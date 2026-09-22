# hack/host-common.ps1 -- shared helpers for the Windows host scripts.
#
# This file is NOT runnable on its own. It is dot-sourced:
#
#   . (Join-Path $PSScriptRoot 'host-common.ps1')
#
# WHY ASCII-ONLY (same reason as hack/verify.ps1, repeated because this file is
# the one people copy from): Windows PowerShell 5.1 reads a .ps1 file WITHOUT a
# BOM as ANSI, so any non-ASCII literal is corrupted at PARSE time. The failure
# mode is brutal to diagnose: the script exits -1 and prints NOTHING, which
# reads like "PowerShell is broken" rather than "the file encoding is wrong".
# Project documentation is Chinese; every .ps1 in this repo intentionally is not.
#
# WHY THESE HELPERS ARE SHARED AT ALL:
#   There are three host scripts that need the same things -- coloured output,
#   the elevation test, and a native-command runner that CANNOT hang. Copying
#   them would guarantee that a fix to one (the timeout, say) is missed in the
#   others. That is the same reasoning that keeps three container images on one
#   Dockerfile.
#
# SCOPING NOTE: dot-sourcing runs this file in the *caller's* scope, so the
# variables below and the counters the writers touch are the caller's
# script-scoped variables. That is deliberate: Write-Bad / Write-Warn must be
# able to influence the caller's exit code.

$script:failed = 0
$script:warned = 0

function Write-Head {
    param([string]$Text)
    Write-Host ''
    Write-Host "=== $Text ===" -ForegroundColor Cyan
}

function Write-OK   { param([string]$t) Write-Host "[ OK ] $t" -ForegroundColor Green }
function Write-Warn { param([string]$t) Write-Host "[WARN] $t" -ForegroundColor Yellow; $script:warned++ }
function Write-Bad  { param([string]$t) Write-Host "[FAIL] $t" -ForegroundColor Red;    $script:failed++ }
function Write-Info { param([string]$t) Write-Host "       $t" }
function Write-Step { param([string]$t) Write-Host "[STEP] $t" -ForegroundColor Cyan }

# Am I running with an elevated token?
#
# This is NOT the same question as "is the user an administrator". A user in the
# Administrators group gets a filtered (medium integrity) token by default, and
# that token is listed as "Group used for deny only". So a check that looks for
# group membership answers "yes" and then DISM still fails with error 740. The
# only honest test is the token's own claim.
function Test-Elevated {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    return ([Security.Principal.WindowsPrincipal]::new($id)).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Win32_OptionalFeature install state, readable WITHOUT elevation:
#   1 = absent, 2 = installed/enabled, 3 = disabled, -1 = the query failed.
function Get-FeatureState {
    param([string]$Name)
    $f = Get-CimInstance Win32_OptionalFeature -Filter "Name='$Name'" -ErrorAction SilentlyContinue
    if ($null -eq $f) { return -1 }
    return [int]$f.InstallState
}

function Test-ServiceRegistered {
    param([string]$Name)
    return ($null -ne (Get-Service -Name $Name -ErrorAction SilentlyContinue))
}

# Invoke-Native: run an external command with a HARD TIMEOUT and return merged
# output plus the exit code.
#
# Why the timeout is not optional: when the Docker named pipe exists but the
# Linux engine is dead, `docker info` does not fail fast -- it blocks. A script
# that hangs while diagnosing a hang is worse than useless, so every external
# call runs in a job and is abandoned after $TimeoutSeconds.
# (Stop-Job tears down the runspace; the orphaned child process is expected to
# exit on its own once the pipe errors out. That is an acceptable trade for
# guaranteeing the caller always terminates.)
#
# The parameter is deliberately NOT named $Args: PowerShell variables are case
# insensitive, so that name would shadow the automatic $args variable.
#
# WorkingDirectory is not decoration either. Start-Job creates a fresh runspace,
# and that runspace does NOT inherit the caller's location -- native commands
# launched from it start in the process's original directory instead. Found the
# hard way: `kubectl apply -k config/crd` inside a job resolved the path against
# C:\Users\<user>\Documents, which reported "not a valid directory" for a path
# that plainly exists. Every relative path in the calling scripts depended on
# this, so it is fixed here rather than worked around in each caller.
function Invoke-Native {
    param(
        [string]$Exe,
        [string[]]$ArgList,
        [int]$TimeoutSeconds = 20,
        # Native tools do NOT all speak the same encoding, and getting this wrong
        # corrupts exactly the line you need to read:
        #   wsl.exe  -> UTF-16LE
        #   docker   -> UTF-8
        #   whoami   -> the console code page
        # PowerShell 5.1 otherwise decodes native stdout using the console code
        # page (GBK on a zh-CN machine), which turns a readable Chinese error
        # message into mojibake.
        [ValidateSet('default', 'utf8', 'unicode')]
        [string]$OutputEncoding = 'default',
        [string]$WorkingDirectory = ''
    )
    if (-not $WorkingDirectory) { $WorkingDirectory = (Get-Location).ProviderPath }
    $job = Start-Job -ScriptBlock {
        param($e, $a, $enc, $wd)
        try {
            if ($wd) { Set-Location -LiteralPath $wd -ErrorAction SilentlyContinue }
            if ($enc -eq 'utf8') {
                [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
            } elseif ($enc -eq 'unicode') {
                [Console]::OutputEncoding = [System.Text.Encoding]::Unicode
            }
            # ForEach-Object { "$_" } matters: piping native stderr into Out-String
            # would render ErrorRecords with the whole "CategoryInfo /
            # NativeCommandError / FullyQualifiedErrorId" block, burying the one
            # line that actually explains the failure. The RemoteException filter
            # drops the bare type name PowerShell tacks onto wrapped stderr records.
            $out = (& $e @a 2>&1 | ForEach-Object { "$_" } |
                Where-Object { $_ -notmatch '^System\.Management\.Automation\.\w+$' }) -join "`n"
            @{ Exit = $LASTEXITCODE; Text = $out.Trim() }
        } catch {
            @{ Exit = -1; Text = $_.Exception.Message }
        }
    } -ArgumentList $Exe, $ArgList, $OutputEncoding, $WorkingDirectory
    if (Wait-Job $job -Timeout $TimeoutSeconds) {
        $result = Receive-Job $job
        Remove-Job $job -Force
        if ($null -eq $result) { return @{ Exit = -1; Text = '' } }
        return $result
    }
    Stop-Job $job -ErrorAction SilentlyContinue
    Remove-Job $job -Force -ErrorAction SilentlyContinue
    return @{ Exit = 124; Text = "timed out after ${TimeoutSeconds}s" }
}

# The CBS state of the packages behind an optional feature.
#
# Why this exists on top of Win32_OptionalFeature: the two answers differ in the
# case that actually bit this project. Win32_OptionalFeature said "not installed"
# while CBS was holding an *install request* that had survived a reboot -- i.e.
# the reboot happened and did not apply the change. The registry value is a
# CbsInstallState with the state number in the high nibble, and the names below
# are the ones DISM itself prints (see C:\Windows\Logs\CBS\CBS.log:
# "CBS state 4(CbsInstallStateStaged)", "CBS state 6(CbsInstallStateInstallRequested)",
# and the servicing log's "state: Installed" for a package reading 0x70).
#
# Returns the NEWEST package matching the name pattern, since several revisions
# are normally present and only the newest one is meaningful.
function Get-CbsPackageState {
    param([string]$NameLike)
    $root = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\Packages'
    $best = $null
    foreach ($p in (Get-ChildItem $root -ErrorAction SilentlyContinue)) {
        if ($p.PSChildName -notlike $NameLike) { continue }
        $props = Get-ItemProperty $p.PSPath -Name CurrentState -ErrorAction SilentlyContinue
        if ($null -eq $props) { continue }
        $revText = $p.PSChildName.Split('~')[-1]
        $rev = $null
        try { $rev = [version]$revText } catch { $rev = [version]'0.0.0.0' }
        if ($null -eq $best -or $rev -gt $best.Rev) {
            $best = [pscustomobject]@{
                Name  = $p.PSChildName
                Rev   = $rev
                State = [int]$props.CurrentState
            }
        }
    }
    return $best
}

# Human name for the raw CbsInstallState seen in the registry (see above).
function Format-CbsState {
    param([int]$State)
    switch ($State) {
        0x40 { 'staged (payload present, not installed)' }
        0x50 { 'uninstall requested (pending)' }
        0x60 { 'install requested (pending)' }
        0x70 { 'installed' }
        default { 'unknown (raw 0x' + $State.ToString('X2') + ')' }
    }
}
