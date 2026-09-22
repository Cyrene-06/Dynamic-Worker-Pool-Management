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
# Shared with hack/host-ready.ps1 and hack/kind-e2e.ps1: the coloured writers
# (Write-Head / Write-OK / Write-Warn / Write-Bad / Write-Info), Test-Elevated,
# Get-FeatureState and Invoke-Native -- the native-command runner that cannot
# hang. One copy on purpose: a fix to the timeout or to the stderr filtering
# must not have to be remembered in three files, and this doctor is only useful
# if its view of the machine matches the repair script's view of it.
. (Join-Path $PSScriptRoot 'host-common.ps1')

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
    Write-Info "wsl.exe: $($wsl.Source) (file version $((Get-Item $wsl.Source).VersionInfo.FileVersion))"

    # wsl.exe EXISTS ON EVERY modern Windows build, even when WSL is not installed
    # at all -- it is the inbox stub that can install WSL. So "wsl.exe is present"
    # must never be read as "WSL works". What proves the runtime is installed:
    #   - the Store package (MicrosoftCorporationII.WindowsSubsystemForLinux), or
    #   - %ProgramFiles%\WSL\wsl.exe (the newer MSIX layout)
    $wslMsix = Get-AppxPackage -Name '*WindowsSubsystemForLinux*' -ErrorAction SilentlyContinue
    $wslMsixExe = Join-Path $env:ProgramFiles 'WSL\wsl.exe'
    if ($wslMsix -or (Test-Path $wslMsixExe)) {
        if ($wslMsix) { Write-OK "WSL runtime package installed: $($wslMsix.Name) $($wslMsix.Version)" }
        if (Test-Path $wslMsixExe) { Write-OK "WSL runtime found: $wslMsixExe" }
    } else {
        Write-Warn 'WSL runtime (the kernel/MSIX) is NOT installed -- only the inbox stub exists.'
        $script:wslRuntimeMissing = $true
    }

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
        Write-Info 'Docker Desktop needs its own distro (docker-desktop), created on its first start.'
    }
}

# ---------------------------------------------------------------------------
Write-Head '6. CPU virtualization and Windows features'
# Everything here is readable WITHOUT elevation (Win32_Processor, Win32_ComputerSystem,
# Win32_OptionalFeature, and the registry reboot flags). An earlier version of this
# script gave up at this point and told the reader to go get admin rights for DISM --
# which is backwards: on this project's dev machine both features were already ENABLED,
# and the only real blocker was a pending reboot. Diagnose first, elevate last.
$cpu = Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1
if ($cpu) {
    Write-Info "CPU: $($cpu.Name)"
    if ($cpu.VirtualizationFirmwareEnabled) {
        Write-OK 'virtualization is enabled in firmware (VT-x/AMD-V).'
    } else {
        Write-Bad 'virtualization is DISABLED in firmware: enable VT-x/AMD-V in BIOS/UEFI.'
        Write-Info 'No amount of Windows configuration can work around this.'
    }
    if ($cpu.SecondLevelAddressTranslationExtensions) {
        Write-OK 'SLAT (EPT/NPT) is available (WSL2 requires it).'
    } else {
        Write-Warn 'SLAT not reported; WSL2 needs it.'
    }
}
$os = Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue
if ($os) { Write-Info "OS: $($os.Caption) build $($os.BuildNumber)" }

foreach ($name in 'Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform') {
    # InstallState: 1 = absent, 2 = installed/enabled, 3 = disabled.
    $f = Get-CimInstance Win32_OptionalFeature -Filter "Name='$name'" -ErrorAction SilentlyContinue
    switch ([int]$f.InstallState) {
        2 {
            Write-OK "$name : enabled (InstallState=2)"
            $script:featuresEnabled = $true
        }
        3 {
            Write-Bad "$name : DISABLED (InstallState=3) -- enable it in an ADMIN shell."
            $script:featuresMissing = $true
        }
        1 {
            Write-Bad "$name : not installed (InstallState=1) -- enable it in an ADMIN shell."
            $script:featuresMissing = $true
        }
        default { Write-Warn "$name : state unknown (verify with elevated DISM /online /get-featureinfo)." }
    }
}

# The trap this section exists for: a feature that is "enabled" in the servicing store
# is NOT ACTIVE until the machine reboots. Enabled + vmcompute missing + a pending
# reboot == the single missing step is a reboot, not another command.
$pending = @()
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') { $pending += 'CBS RebootPending' }
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired') { $pending += 'WindowsUpdate RebootRequired' }
$pfr = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager' -Name PendingFileRenameOperations -ErrorAction SilentlyContinue).PendingFileRenameOperations
if ($pfr) { $pending += "PendingFileRenameOperations=$($pfr.Count)" }
if ($pending.Count -gt 0) {
    Write-Warn "a reboot is PENDING ($($pending -join ', ')): enable feature changes do not take effect until then."
    $script:rebootPending = $true
} else {
    Write-OK 'no pending reboot detected.'
}

if ((Get-CimInstance Win32_ComputerSystem -ErrorAction SilentlyContinue).HypervisorPresent) {
    Write-OK 'the Windows hypervisor is running.'
} else {
    Write-Warn 'the Windows hypervisor is NOT running (expected when the platform feature is enabled but not yet active).'
    $script:hypervisorDown = $true
}

# ---------------------------------------------------------------------------
Write-Head '7. Windows servicing (why a reboot can fail to apply a change)'
# This section exists because on this machine a reboot did NOT fix it.
#
# The sequence: DISM was asked to enable VirtualMachinePlatform, it answered
# "Reboot required=yes", the machine was rebooted -- and the feature was still
# absent. None of that is visible from the outside. The only record is
# C:\Windows\Logs\CBS\CBS.log:
#     Startup: Deferring startup processing at users request
#     Aborted processing startup actions, requiring shutdown processing and
#     will try startup processing again in the future.
# "The reboot did not apply it" is therefore a real, nameable state, and this
# section names it, instead of letting the reader conclude DISM is broken.
#
# Four signals, all readable WITHOUT elevation:
#   - the CBS state of the VirtualMachinePlatform payload. 0x60 means
#     "install requested" -- an install that a reboot was supposed to apply.
#     Still 0x60 after a reboot = that reboot did not take.
#   - the CBS RebootPending marker.
#   - TrustedInstaller's start type. If it is not Automatic, CBS logs "No startup
#     processing required" and the pending work waits forever.
#   - fast startup. With it on, "shut down" + power on is a RESUME, not a boot,
#     and pending servicing has no startup path to run on.
$vmp = Get-CbsPackageState 'Microsoft-Windows-HyperV-OptionalFeature-VirtualMachinePlatform-Client-Package~*~amd64~~*'
if ($null -eq $vmp) {
    Write-Warn 'no CBS payload entry found for VirtualMachinePlatform.'
} else {
    $stateText = Format-CbsState $vmp.State
    if ($vmp.State -eq 0x60) {
        Write-Warn "VirtualMachinePlatform payload: $stateText"
        Write-Info 'If the machine has already been rebooted since DISM said "reboot required",'
        Write-Info 'then that reboot did NOT apply it (see step 1 of the fix sequence below).'
        $script:servicingStuck = $true
    } elseif ($vmp.State -eq 0x70) {
        Write-OK "VirtualMachinePlatform payload: $stateText"
    } else {
        Write-Info "VirtualMachinePlatform payload: $stateText"
    }
}

if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') {
    Write-Warn 'CBS still has a pending reboot marker.'
    $script:rebootPending = $true
}

$tiStart = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\TrustedInstaller' -Name Start -ErrorAction SilentlyContinue).Start
if ($null -eq $tiStart) {
    Write-Warn 'the TrustedInstaller service is not registered (unusual).'
} elseif ($tiStart -eq 2) {
    Write-OK 'TrustedInstaller start type is Automatic: CBS runs startup processing at boot.'
} else {
    Write-Bad "TrustedInstaller start type is $tiStart (2 = Automatic): pending servicing will never run."
}

$hiberboot = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power' -Name HiberbootEnabled -ErrorAction SilentlyContinue).HiberbootEnabled
if ($hiberboot -eq 1) {
    Write-Warn 'fast startup is ON: "shut down" + power on is a resume, not a boot.'
    Write-Info 'After asking Windows to enable a feature, use Restart (or "shutdown /r").'
}

$cbsLog = Join-Path $env:WINDIR 'Logs\CBS\CBS.log'
if (Test-Path $cbsLog) {
    $defer = Get-Content $cbsLog -ErrorAction SilentlyContinue |
        Select-String -Pattern 'Deferring startup processing' | Select-Object -Last 1
    if ($defer) {
        Write-Warn 'CBS deferred startup processing at the last startup:'
        Write-Info ('  ' + $defer.Line.Trim())
        $script:servicingStuck = $true
    } else {
        Write-OK 'CBS never deferred startup processing in the current log.'
    }
} else {
    Write-Info "CBS log not readable: $cbsLog"
}

# ---------------------------------------------------------------------------
Write-Head '8. Group membership'
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
Write-Host 'blocked until this is fixed.' -ForegroundColor Yellow
Write-Host ''
Write-Host 'Fix sequence, ordered by what THIS machine is missing (every step needs' -ForegroundColor Yellow
Write-Host 'administrator rights and/or a reboot, so none of it can be automated here):' -ForegroundColor Yellow
Write-Host ''

$step = 1

# Order the advice by what is ACTUALLY missing. Reporting "reboot first because the
# features are already enabled" was wrong on this machine after a reboot: the
# VirtualMachinePlatform feature had gone back to 'not installed' (InstallState=1),
# so the reboot could not possibly have helped. Check the feature state first.
if ($script:featuresMissing) {
    Write-Host "  $step. Enable the missing Windows features (ADMIN), then RESTART." -ForegroundColor Cyan
    Write-Host '     Without VirtualMachinePlatform there is no hns/vmcompute, no running'
    Write-Host '     hypervisor, and therefore no WSL2 distro and no Docker engine --'
    Write-Host '     the WSL runtime being installed is NOT enough on its own.'
    Write-Host '     Easiest: one script does this whole step set (re-arm the request, turn'
    Write-Host '     fast startup off, put TrustedInstaller back to automatic, then tell you'
    Write-Host '     to restart):'
    Write-Host '       powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1 -Mode repair'
    Write-Host '     By hand it is:'
    Write-Host '       DISM /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart'
    Write-Host '       DISM /online /enable-feature /featurename:Microsoft-Windows-Subsystem-Linux /all /norestart'
    Write-Host '     Then RESTART -- never "shut down" + power on, which may be a resume'
    Write-Host '     of a hibernated session and applies nothing:'
    Write-Host '       shutdown /r /t 0'
    if ($script:servicingStuck) {
        Write-Host ''
        Write-Host '     NOTE: a reboot has already failed to apply this once (section 7).' -ForegroundColor Yellow
        Write-Host '     That is not a reason to give up and it is not "DISM is broken": a'
        Write-Host '     deferred servicing operation is retried at the next boot. Re-run the'
        Write-Host '     enable command above (it re-arms the request) and restart again.'
        Write-Host '     If a SECOND restart still leaves it pending, the servicing stack is'
        Write-Host '     the suspect. Check Windows Update for a staged update first (a staged'
        Write-Host '     cumulative update and a feature request can block each other), then:'
        Write-Host '       DISM /online /cleanup-image /restorehealth     (ELEVATED)'
        Write-Host '       sfc /scannow                                    (ELEVATED)'
    }
    Write-Host ''
    $step++
} elseif ($script:rebootPending) {
    Write-Host "  $step. REBOOT FIRST." -ForegroundColor Cyan
    Write-Host '     The required features are enabled but a reboot is pending, and an'
    Write-Host '     enabled-but-not-active feature looks exactly like a missing one from'
    Write-Host '     the outside: no hns, no vmcompute, no distro.'
    Write-Host ''
    $step++
}

if ($script:wslRuntimeMissing) {
    Write-Host "  $step. Install the WSL2 runtime (ADMIN PowerShell)." -ForegroundColor Cyan
    Write-Host '       wsl --install --no-distribution'
    Write-Host '     --no-distribution is deliberate: Docker Desktop brings its own distro'
    Write-Host '     (docker-desktop); a spare Ubuntu would only eat disk and RAM.'
    Write-Host '     Restricted network / Store disabled? Then install the MSI offline:'
    Write-Host '       https://github.com/microsoft/WSL/releases   (wsl.<version>.x64.msi)'
    Write-Host '     and fall back to: wsl --update'
    Write-Host ''
    $step++
} else {
    Write-Host "  $step. Refresh/verify the WSL runtime (ADMIN PowerShell)." -ForegroundColor Cyan
    Write-Host '       wsl --update'
    Write-Host '       wsl --set-default-version 2'
    Write-Host ''
    $step++
}

Write-Host "  $step. Start Docker Desktop once, as the NORMAL user (NOT elevated), and" -ForegroundColor Cyan
Write-Host '     wait until it reports "Docker Desktop is running". Its first start'
Write-Host '     creates the docker-desktop distro and registers com.docker.service.'
Write-Host '     Deliberately not "as administrator": Docker Desktop keeps per-user'
Write-Host '     state (settings-store.json, the WSL distro, the named pipes) and an'
Write-Host '     elevated start produces a second, differently-owned set of them, which'
Write-Host '     then looks like "the engine is up but the CLI cannot reach it".'
Write-Host '     This is the one step hack/host-ready.ps1 will not do for you.'
Write-Host ''
$step++

Write-Host "  $step. Verify (this order tells you WHICH layer is still broken):" -ForegroundColor Cyan
Write-Host '       wsl --status'
Write-Host '       wsl --list --verbose'
Write-Host '       docker info          # must not hang; re-run this script if it does'
Write-Host '     Then, once the daemon answers, the two commands that were never'
Write-Host '     executable on this machine:'
Write-Host '       make docker-build'
Write-Host '       powershell -ExecutionPolicy Bypass -File hack/kind-e2e.ps1 -Stage cluster,image,load,deploy,verify'
Write-Host ''
Write-Host '  Notes:'
Write-Host '    - hack/host-ready.ps1 walks these same steps and applies them; this'
Write-Host '      script only reports. Run the doctor first, the repair script second.'
Write-Host '    - On Windows Home there is no Hyper-V: the WSL2 backend is the only option.'
Write-Host '    - "wsl.exe exists" never means "WSL is installed" -- every modern Windows'
Write-Host '      ships it as an installer stub.'
Write-Host ''
exit 1
