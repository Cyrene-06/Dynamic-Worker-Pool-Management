# hack/docker-doctor.ps1 -- diagnose why the local container runtime is unusable.
#
# WHY THIS FILE IS ASCII-ONLY:
#   Same reason as hack/verify.ps1 (see its header). Windows PowerShell 5.1 parses
#   a .ps1 file WITHOUT a BOM as ANSI, so any non-ASCII literal is corrupted at
#   PARSE time and the failure mode is "exit -1 with zero output", which reads
#   like "PowerShell is broken" rather than "the file encoding is wrong".
#   Diagnostics are printed in English for the same reason. Project docs are in
#   Chinese; this script intentionally is not.
#
# WHAT IT DOES / DOES NOT DO:
#   It is READ-ONLY and never elevates. Fixing the runtime needs administrator
#   rights plus a reboot, and this agent/tool session cannot answer a UAC prompt,
#   so the script prints the exact commands to run instead of trying to run them.
#
# WHY IT EXISTS AT ALL:
#   "docker version" failing with an HTTP 500 from the Docker Desktop Linux engine
#   looks like "Docker is broken". The actual causes are always mundane and
#   local: the service is not registered, WSL2 / VirtualMachinePlatform is not
#   enabled, or Docker Desktop has never completed first-run initialization.
#   Naming the failing layer is the whole point.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File hack/docker-doctor.ps1
#
# Exit codes: 0 = daemon reachable. 1 = CLI missing or daemon unreachable.
# Note: only kind-based end-to-end runs need Docker. Unit tests and the envtest
#       suite do not (see README 2.6), so a failing exit code here is not a
#       blocker for day-to-day development.

param(
    [switch]$Full
)

$ErrorActionPreference = 'Continue'
$script:failed = 0
$script:warned = 0

function Write-Head {
    param([string]$Text)
    Write-Host ''
    Write-Host "=== $Text ===" -ForegroundColor Cyan
}

function Write-OK   { param([string]$t) Write-Host "[ OK ] $t"   -ForegroundColor Green }
function Write-Warn { param([string]$t) Write-Host "[WARN] $t"   -ForegroundColor Yellow; $script:warned++ }
function Write-Bad  { param([string]$t) Write-Host "[FAIL] $t"   -ForegroundColor Red;    $script:failed++ }
function Write-Info { param([string]$t) Write-Host "       $t" }

# Invoke-Native: run an external command with a HARD TIMEOUT and return merged
# output plus the exit code.
#
# Why the timeout is not optional: when the Docker named pipe exists but the
# Linux engine is dead, `docker info` does not fail fast -- it blocks. A doctor
# script that hangs while diagnosing a hang is worse than useless, so every
# external call here runs in a job and is abandoned after $TimeoutSeconds.
# (Stop-Job tears down the runspace; the orphaned child process is expected to
# exit on its own once the pipe errors out. That is an acceptable trade for
# guaranteeing this script always terminates.)
#
# The parameter is deliberately NOT named $Args: PowerShell variables are case
# insensitive, so that name would shadow the automatic $args variable.
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
        [string]$OutputEncoding = 'default'
    )
    $job = Start-Job -ScriptBlock {
        param($e, $a, $enc)
        try {
            if ($enc -eq 'utf8') {
                [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
            } elseif ($enc -eq 'unicode') {
                [Console]::OutputEncoding = [System.Text.Encoding]::Unicode
            }
            # ForEach-Object { "$_" } matters: piping native stderr into Out-String
            # would render ErrorRecords with the whole "CategoryInfo /
            # NativeCommandError / FullyQualifiedErrorId" block, burying the one
            # line that actually explains the failure.
            $out = (& $e @a 2>&1 | ForEach-Object { "$_" }) -join "`n"
            @{ Exit = $LASTEXITCODE; Text = $out.Trim() }
        } catch {
            @{ Exit = -1; Text = $_.Exception.Message }
        }
    } -ArgumentList $Exe, $ArgList, $OutputEncoding
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

$daemonReachable = $false

# ---------------------------------------------------------------------------
Write-Head '1. Docker CLI'
$dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
if (-not $dockerCmd) {
    Write-Bad 'docker.exe not found on PATH.'
    Write-Info 'Install Docker Desktop: https://docs.docker.com/desktop/setup/install/windows-install/'
} else {
    Write-OK "docker CLI: $($dockerCmd.Source)"
    $v = Invoke-Native docker @('--version') 20 'utf8'
    Write-Info $v.Text
    if (-not (Get-Command kind -ErrorAction SilentlyContinue)) {
        Write-Warn 'kind not found on PATH (needed for make kind-up / make kind-load).'
    } else {
        Write-OK 'kind CLI found.'
    }
}

# ---------------------------------------------------------------------------
Write-Head '2. Docker daemon (this is the check that usually fails)'
if ($dockerCmd) {
    $info = Invoke-Native docker @('info', '--format', '{{.ServerVersion}}') 15 'utf8'
    if ($info.Exit -eq 0 -and $info.Text) {
        Write-OK "daemon reachable, server version $($info.Text)"
        $daemonReachable = $true
    } elseif ($info.Exit -eq 124) {
        Write-Bad 'docker info HUNG and had to be abandoned after 15s.'
        Write-Info 'The named pipe exists but nothing answers on it, which is exactly'
        Write-Info 'what a half-initialized Docker Desktop looks like.'
    } else {
        Write-Bad 'daemon NOT reachable.'
        if ($info.Text) {
            $firstLine = ($info.Text -split "`n")[0].Trim()
            Write-Info "first line of the error: $firstLine"
        }
        Write-Info 'An HTTP 500 from dockerDesktopLinuxEngine means the CLI is fine'
        Write-Info 'but the Linux engine (i.e. the WSL2 backend) is not running.'
    }
} else {
    Write-Warn 'skipped: no docker CLI.'
}

# ---------------------------------------------------------------------------
Write-Head '3. Docker Desktop installation'
# Two probing strategies, because Docker Desktop has two install flavors:
#   - machine-wide: %ProgramFiles%\Docker\Docker\Docker Desktop.exe
#   - per-user (newer installer): %LOCALAPPDATA%\Programs\DockerDesktop\...
# The CLI path itself is the most reliable evidence, so it is used first:
# <install root>\resources\bin\docker.exe
$ddFound = $false
if ($dockerCmd) {
    $cli = $dockerCmd.Source
    $cliRoot = Split-Path (Split-Path (Split-Path $cli -Parent) -Parent) -Parent
    Write-Info "install root inferred from the CLI: $cliRoot"
    $exe = Join-Path $cliRoot 'Docker Desktop.exe'
    if (Test-Path $exe) { Write-OK "found: $exe"; $ddFound = $true }
}
# Both candidate paths are guarded: Join-Path throws a binding error on a null
# Path, and that would abort the script before it prints anything useful.
if (-not $ddFound) {
    $candidates = @()
    if ($env:ProgramFiles) { $candidates += (Join-Path $env:ProgramFiles 'Docker\Docker\Docker Desktop.exe') }
    if (${env:ProgramFiles(x86)}) { $candidates += (Join-Path ${env:ProgramFiles(x86)} 'Docker\Docker\Docker Desktop.exe') }
    foreach ($p in $candidates) {
        if (Test-Path $p) { Write-OK "found: $p"; $ddFound = $true }
    }
}
if (-not $ddFound) {
    Write-Warn 'Docker Desktop.exe not found in the default locations.'
    Write-Info 'Check where the CLI actually points: Get-Command docker'
}
# Docker Desktop writes this file on first successful initialization. Its absence
# is a strong signal that the app has never finished starting up.
$settings = Join-Path $env:APPDATA 'Docker\settings-store.json'
if (Test-Path $settings) {
    Write-OK "first-run settings found: $settings"
} else {
    Write-Warn "first-run settings NOT found: $settings"
    Write-Info 'Docker Desktop has probably never completed initialization.'
}

# Process state disambiguates the two very different "daemon unreachable" cases:
#   - no com.docker.* process  -> Docker Desktop is simply not running
#   - com.docker.backend alive -> the app is up but its engine/WSL backend is not
$ddProcs = Get-Process -ErrorAction SilentlyContinue |
    Where-Object { $_.ProcessName -like 'com.docker*' -or $_.ProcessName -eq 'docker' }
if ($ddProcs) {
    foreach ($p in $ddProcs) { Write-Info "running: $($p.ProcessName) (pid $($p.Id))" }
} else {
    Write-Info 'no com.docker.* / docker process is running.'
}

# ---------------------------------------------------------------------------
Write-Head '4. Required services'
# com.docker.service  : the Docker Desktop privileged helper service
# vmcompute           : Hyper-V Host Compute Service (needed by the WSL2 backend)
# hns                 : Host Network Service (container networking)
# LxssManager         : WSL1 sessions (harmless if unused, but tells us WSL is there)
foreach ($name in @('com.docker.service', 'vmcompute', 'hns', 'LxssManager')) {
    $svc = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($null -eq $svc) {
        Write-Warn "service '$name' is not registered."
    } else {
        Write-Info "$name : $($svc.Status)"
    }
}

# ---------------------------------------------------------------------------
Write-Head '5. WSL'
$wsl = Get-Command wsl -ErrorAction SilentlyContinue
if (-not $wsl) {
    Write-Bad 'wsl.exe not found: WSL is not installed (the WSL2 backend cannot work).'
} else {
    $st = Invoke-Native wsl @('--status') 20 'unicode'
    if ($st.Exit -eq 0) {
        Write-OK 'wsl --status succeeded.'
        if ($Full) { $st.Text -split "`n" | ForEach-Object { Write-Info $_.Trim() } }
    } else {
        Write-Bad 'wsl --status failed (WSL installed but not usable).'
        Write-Info $st.Text
    }
    # NOTE: wsl.exe output is localized, so the text is NOT pattern-matched for
    # English strings -- that would silently stop working on a zh-CN machine.
    # The exit code is the only language-independent signal available here.
    $lv = Invoke-Native wsl @('--list', '--verbose') 20 'unicode'
    if ($lv.Exit -eq 0) {
        Write-OK 'wsl --list --verbose succeeded (at least one distribution is installed).'
        if ($Full) { $lv.Text -split "`n" | ForEach-Object { Write-Info $_.Trim() } }
    } else {
        Write-Warn 'no usable WSL distribution (wsl --list --verbose failed).'
        Write-Info 'Docker Desktop needs its own distro (docker-desktop);'
        Write-Info 'run: wsl --install --no-distribution   (as administrator), then reboot.'
    }
}

# ---------------------------------------------------------------------------
Write-Head '6. Virtualization'
# Get-WindowsOptionalFeature needs elevation; report it as "unknown" rather than
# guessing, because a wrong "OK" here would send the reader down the wrong path.
try {
    $feat = Get-WindowsOptionalFeature -Online -FeatureName 'Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform' -ErrorAction Stop
    foreach ($f in $feat) { Write-Info "$($f.FeatureName) : $($f.State)" }
} catch {
    Write-Warn 'cannot read optional features (needs an elevated session).'
    Write-Info 'In an ADMIN PowerShell run:'
    Write-Info '  DISM /online /get-featureinfo /featurename:Microsoft-Windows-Subsystem-Linux'
    Write-Info '  DISM /online /get-featureinfo /featurename:VirtualMachinePlatform'
}

# ---------------------------------------------------------------------------
Write-Head '7. Group membership'
# Missing docker-users shows up as "access denied" on the named pipe rather than
# as an obvious permission error, so it is worth stating explicitly.
$groups = Invoke-Native whoami @('/groups')
if ($groups.Text -match 'docker-users') {
    Write-OK 'current user is in the docker-users group.'
} else {
    Write-Warn 'current user is NOT in docker-users (may cause access-denied on the pipe).'
    Write-Info 'Add it in an ADMIN session: net localgroup docker-users "%USERNAME%" /add'
}

# ---------------------------------------------------------------------------
Write-Head 'Result'
if ($daemonReachable) {
    Write-Host 'Daemon reachable. You can run:' -ForegroundColor Green
    Write-Host '  make docker-build' -ForegroundColor Green
    Write-Host '  make kind-up' -ForegroundColor Green
    Write-Host '  make kind-load' -ForegroundColor Green
    exit 0
}

Write-Host 'Daemon NOT reachable. Scripts and unit tests are unaffected (they do not' -ForegroundColor Yellow
Write-Host 'need a container runtime), but image builds and kind end-to-end runs are' -ForegroundColor Yellow
Write-Host 'blocked until this is fixed. Fix sequence (ALL steps need ADMIN plus a' -ForegroundColor Yellow
Write-Host 'reboot, so it cannot be automated from here):' -ForegroundColor Yellow
Write-Host ''
Write-Host '  1. Enable the WSL2 prerequisites (admin PowerShell):'
Write-Host '       wsl --install --no-distribution'
Write-Host '     If wsl.exe is missing entirely, enable the features first:'
Write-Host '       DISM /online /enable-feature /featurename:Microsoft-Windows-Subsystem-Linux /all /norestart'
Write-Host '       DISM /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart'
Write-Host ''
Write-Host '  2. REBOOT. Skipping this leaves vmcompute unregistered and the same 500 comes back.'
Write-Host ''
Write-Host '  3. Start Docker Desktop once, as administrator, and wait until it reports'
Write-Host '     "Docker Desktop is running" (first start initializes the docker-desktop distro).'
Write-Host ''
Write-Host '  4. Verify, then re-run this script:'
Write-Host '       wsl --set-default-version 2'
Write-Host '       docker info'
Write-Host ''
Write-Host '  Note: on Windows Home the WSL2 backend is the only option (no Hyper-V).'
Write-Host '        Docker Desktop reports a missing virtualization feature explicitly.'
Write-Host ''
exit 1
