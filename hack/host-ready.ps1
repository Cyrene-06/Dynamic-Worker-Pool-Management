# hack/host-ready.ps1 -- make this Windows machine able to run Linux containers.
#
# Companion to hack/docker-doctor.ps1, and the split is deliberate:
#   docker-doctor.ps1  is READ-ONLY. Safe to run any time, needs no elevation,
#                      and explains WHICH layer is broken.
#   host-ready.ps1     CHANGES machine state. It refuses to do anything without
#                      administrator rights, and it is the script that actually
#                      fixes the layers the doctor complains about.
#
# WHY IT IS ASCII-ONLY: see hack/verify.ps1. PS 5.1 reads a BOM-less .ps1 as
# ANSI, so a non-ASCII literal breaks at PARSE time and the script exits -1 with
# zero output. Project docs are Chinese; every .ps1 in this repo is not.
#
# THE CASE THIS WAS WRITTEN FOR (do not "simplify" it away):
#   On this checkout's dev machine VirtualMachinePlatform was absent and the WSL2
#   runtime was already installed. Docker Desktop was installed but its Linux
#   engine had never come up, so `docker info` had no answer and image builds and
#   kind had never been executed even once.
#   The obvious fix was run ...
#       DISM /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart
#   ... which answered "Reboot required". The machine WAS rebooted. The feature
#   was still absent afterwards. C:\Windows\Logs\CBS\CBS.log says why:
#       Startup: Deferring startup processing at users request
#       Aborted processing startup actions, requiring shutdown processing and
#       will try startup processing again in the future.
#   That is the whole trap: "reboot required" is not the same statement as
#   "this reboot will apply it", and nothing on the outside distinguishes the
#   two. So this script does not assume one reboot is enough. It re-arms the
#   request, removes the two things that make a skipped boot likely, and tells
#   you when the state has not moved after a reboot has actually happened.
#
# THE TWO THINGS THAT MAKE A SKIPPED BOOT LIKELY, both handled below:
#   1. Fast startup (hiberboot). With it on, "shut down" + power on is a RESUME
#      of a hibernated session, not a boot, and pending servicing has no startup
#      path to run on. A full "Restart" (or `shutdown /r`) is required. We turn
#      fast startup off so that the distinction stops mattering.
#   2. TrustedInstaller not set to Automatic. CBS then logs "No startup
#      processing required, TrustedInstaller service was not set as autostart"
#      and pending work waits forever, looking exactly like a broken DISM.
#
# USAGE -- one step per run, the script tells you when to stop:
#   powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1
#       read-only report; no elevation needed.
#   powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1 -Mode repair
#       must be started from an ELEVATED shell (the script checks and refuses).
#       Applies the next missing step, then either exits 10 ("reboot now") or
#       continues on to the WSL and Docker layers.
#   powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1 -StartDocker
#       additionally launches Docker Desktop and waits for the engine. Only
#       honoured when NOT elevated -- see the note in step 6.
#
# EXIT CODES:
#   0  host ready: the Docker daemon answered.
#   10 a reboot is required before anything else can help.
#   1  a step failed, or -Mode repair was requested without elevation.
#
# WHAT IT DOES NOT DO: it never touches Hyper-V, never creates or deletes WSL
# distributions, never changes Windows Update settings, and never edits the
# uninstall/repair state of Docker Desktop. Those are not this script's call.

param(
    [ValidateSet('check', 'repair')]
    [string]$Mode = 'check',

    # Launch Docker Desktop and wait for the engine. Opt-in, because starting a
    # GUI app has side effects a "fix my host" script should not have by default.
    [switch]$StartDocker,

    [int]$DockerTimeoutSeconds = 240
)

. (Join-Path $PSScriptRoot 'host-common.ps1')

# Every exit goes through this. An unelevated -Mode repair is a FAILURE even when
# the read-only checks that follow would have been happy, and without a single
# exit point that distinction is one forgotten branch away from being lost.
function Get-ExitCode {
    param([int]$Code)
    if ($script:refusedRepair) { return 1 }
    return $Code
}

$elevated = Test-Elevated
$script:refusedRepair = $false

# The state file is how the script distinguishes "you have not rebooted yet" from
# "you rebooted and the pending install was skipped anyway". Without it the
# second run would print the same "reboot now" line forever and read as a loop.
$statePath = Join-Path $env:TEMP 'dwp-host-ready.state'
$lastBoot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
$lastBootText = $lastBoot.ToString('o')
$previousBoot = $null
if (Test-Path $statePath) {
    $previousBoot = (Get-Content $statePath -Raw -ErrorAction SilentlyContinue)
    if ($previousBoot) { $previousBoot = $previousBoot.Trim() }
}
$rebootAlreadyHappened = ($previousBoot -and $previousBoot -ne $lastBootText)

$requiredFeatures = @('VirtualMachinePlatform', 'Microsoft-Windows-Subsystem-Linux')

Write-Head '0. Context'
Write-Info "mode      : $Mode"
Write-Info "elevated  : $elevated"
Write-Info "last boot : $lastBootText"
if ($rebootAlreadyHappened) {
    Write-Info 'a reboot HAS happened since the last run of this script.'
} elseif ($previousBoot) {
    Write-Info 'no reboot since the last run of this script.'
}
if ($Mode -eq 'repair' -and -not $elevated) {
    Write-Bad '-Mode repair needs an ELEVATED shell: NOTHING will be changed.'
    Write-Info 'Start menu -> "Windows PowerShell" -> right click -> Run as administrator'
    Write-Info 'or, from a normal shell:'
    Write-Info '  Start-Process powershell -Verb RunAs -ArgumentList ''-ExecutionPolicy Bypass -File hack\host-ready.ps1 -Mode repair'''
    # Continue read-only rather than exiting here: the one thing the user needs in
    # the same breath as "you are not elevated" is WHAT is missing.
    Write-Info 'Continuing read-only so this run still tells you what is missing.'
    $script:refusedRepair = $true
    $Mode = 'check'
}

# ---------------------------------------------------------------------------
Write-Head '1. Windows features'
$missing = @()
foreach ($name in $requiredFeatures) {
    switch (Get-FeatureState $name) {
        2 { Write-OK "$name : enabled" }
        1 { Write-Warn "$name : NOT installed"; $missing += $name }
        3 { Write-Warn "$name : DISABLED"; $missing += $name }
        default { Write-Warn "$name : state could not be read" }
    }
}

if ($missing.Count -gt 0 -and $Mode -eq 'repair') {
    $dism = Join-Path $env:WINDIR 'System32\dism.exe'
    foreach ($name in $missing) {
        Write-Step "enabling $name (DISM, /norestart; this takes a minute)"
        # 600s: enabling a feature can take far longer than the 20s default when
        # the payload has to be assembled, and a timeout here would leave the
        # machine in a half-requested state that is hard to read afterwards.
        $r = Invoke-Native $dism @('/online', '/enable-feature', "/featurename:$name", '/all', '/norestart') 600 'utf8'
        # 0 = applied in place, 3010 = ERROR_SUCCESS_REBOOT_REQUIRED, which is
        # the normal answer for these two features. Anything else is a real
        # failure and its text is printed rather than summarised.
        if ($r.Exit -eq 0 -or $r.Exit -eq 3010) {
            Write-OK "$name : change accepted (dism exit $($r.Exit))"
        } else {
            Write-Bad "$name : DISM failed (exit $($r.Exit))"
            Write-Info $r.Text
        }
    }
    # Re-read instead of trusting the exit code: DISM saying "accepted" is not
    # the same as the feature being on, and this is the one place where the
    # difference decides what the user does next.
    $stillMissing = @()
    foreach ($name in $requiredFeatures) {
        if ((Get-FeatureState $name) -ne 2) { $stillMissing += $name }
    }
    if ($stillMissing.Count -eq 0) {
        Write-OK 'all required features are enabled now (no reboot needed for them).'
    } else {
        $missing = $stillMissing
    }
}

# ---------------------------------------------------------------------------
Write-Head '2. Fast startup'
# Readable without elevation; only the fix needs admin.
$powerKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power'
$hiberboot = (Get-ItemProperty $powerKey -Name HiberbootEnabled -ErrorAction SilentlyContinue).HiberbootEnabled
if ($null -eq $hiberboot) {
    Write-Info 'HiberbootEnabled is not set (default: fast startup off on this edition).'
} elseif ($hiberboot -eq 0) {
    Write-OK 'fast startup is OFF: every "shut down" is a real boot.'
} else {
    Write-Warn 'fast startup is ON: "shut down" + power on is a RESUME, and pending servicing is skipped.'
    if ($Mode -eq 'repair') {
        Set-ItemProperty $powerKey -Name HiberbootEnabled -Value 0
        Write-OK 'fast startup disabled (HiberbootEnabled=0). Re-enable with value 1 if you want it back.'
    } else {
        Write-Info 'fix: set HiberbootEnabled=0 (this script does it in -Mode repair)'
    }
}

# ---------------------------------------------------------------------------
Write-Head '3. TrustedInstaller start type'
$tiKey = 'HKLM:\SYSTEM\CurrentControlSet\Services\TrustedInstaller'
$tiStart = (Get-ItemProperty $tiKey -Name Start -ErrorAction SilentlyContinue).Start
if ($null -eq $tiStart) {
    Write-Warn 'the TrustedInstaller service is not registered (unusual).'
} elseif ($tiStart -eq 2) {
    Write-OK 'TrustedInstaller starts automatically: CBS will process pending work at boot.'
} else {
    Write-Warn "TrustedInstaller start type is $tiStart (2 = automatic): pending servicing will never run."
    if ($Mode -eq 'repair') {
        Set-ItemProperty $tiKey -Name Start -Value 2
        Write-OK 'TrustedInstaller set back to automatic (2).'
    } else {
        Write-Info 'fix: set the service Start value to 2 (this script does it in -Mode repair)'
    }
}

# ---------------------------------------------------------------------------
Write-Head '4. Verdict on the feature layer'
if ($missing.Count -gt 0) {
    Write-Bad ("still not enabled: " + ($missing -join ', '))
    if ($Mode -ne 'repair') {
        Write-Info 'fix (ELEVATED): powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1 -Mode repair'
    } else {
        Write-Host ''
        Write-Host 'A reboot is required. Use RESTART, not "shut down" + power on:' -ForegroundColor Yellow
        Write-Host '    shutdown /r /t 0' -ForegroundColor Yellow
        Write-Host 'then run this script again.' -ForegroundColor Yellow
    }
    if ($rebootAlreadyHappened) {
        # This is the stuck case: the reboot happened and Windows did not apply
        # the request. Naming it is the whole point of the state file.
        $vmp = Get-CbsPackageState 'Microsoft-Windows-HyperV-OptionalFeature-VirtualMachinePlatform-Client-Package~*~amd64~~*'
        Write-Host ''
        Write-Host 'WARNING: you rebooted and the feature is STILL missing.' -ForegroundColor Yellow
        Write-Host 'That means the pending install was not applied at startup. Evidence:' -ForegroundColor Yellow
        if ($vmp) {
            Write-Info ("CBS package state: " + (Format-CbsState $vmp.State))
        }
        if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') {
            Write-Info 'CBS still has a pending reboot marker.'
        }
        $cbsLog = Join-Path $env:WINDIR 'Logs\CBS\CBS.log'
        $deferLine = $null
        if (Test-Path $cbsLog) {
            $deferLine = (Get-Content $cbsLog -ErrorAction SilentlyContinue |
                Select-String -Pattern 'Deferring startup processing' | Select-Object -Last 1)
        }
        if ($deferLine) {
            Write-Info 'CBS asked for a deferral at the last startup:'
            Write-Info ('  ' + $deferLine.Line.Trim())
        }
        Write-Host ''
        Write-Host 'Next steps, in this order:' -ForegroundColor Yellow
        Write-Host '  1. Re-run this script with -Mode repair (it re-arms the request) and REBOOT again.' -ForegroundColor Yellow
        Write-Host '     A pending servicing operation that was deferred once is not lost; it needs another boot.' -ForegroundColor Yellow
        Write-Host '  2. If a second reboot still does not move it, the servicing stack itself is suspect.' -ForegroundColor Yellow
        Write-Host '     Check for a pending Windows update first (Settings -> Windows Update); a staged' -ForegroundColor Yellow
        Write-Host '     cumulative update and a feature request can block each other. Then: ' -ForegroundColor Yellow
        Write-Host '       DISM /online /cleanup-image /restorehealth     (ELEVATED, needs Windows Update access)' -ForegroundColor Yellow
        Write-Host '       sfc /scannow                                    (ELEVATED)' -ForegroundColor Yellow
    }
    # Remember the boot time so the NEXT run knows whether a reboot happened.
    $lastBootText | Out-File -FilePath $statePath -Encoding ascii -NoNewline
    exit (Get-ExitCode 10)
}

Write-OK 'VirtualMachinePlatform and the WSL subsystem feature are both enabled.'

# ---------------------------------------------------------------------------
Write-Head '5. WSL runtime'
$wsl = Get-Command wsl -ErrorAction SilentlyContinue
if (-not $wsl) {
    Write-Bad 'wsl.exe not found: the WSL runtime is not installed at all.'
    if ($Mode -eq 'repair') {
        Write-Step 'installing the WSL runtime (no distribution -- Docker Desktop brings its own)'
        # Note "--no-distribution" is deliberate: Docker Desktop creates its own
        # (docker-desktop); a spare Ubuntu would only eat disk and RAM, and it
        # would make "which distro does the engine use" ambiguous.
        $r = Invoke-Native 'wsl' @('--install', '--no-distribution') 900 'unicode'
        Write-Info $r.Text
    } else {
        Write-Info 'fix (ELEVATED): wsl --install --no-distribution'
    }
} else {
    $status = Invoke-Native wsl @('--status') 30 'unicode'
    if ($status.Exit -eq 0) {
        Write-OK 'wsl --status succeeded.'
    } else {
        Write-Warn 'wsl --status failed.'
        Write-Info $status.Text
    }
    # The runtime is an MSIX now, so `wsl.exe` existing proves nothing: every
    # modern Windows ships it as an installer stub.
    $msix = Get-AppxPackage -Name '*WindowsSubsystemForLinux*' -ErrorAction SilentlyContinue
    if ($msix -or (Test-Path (Join-Path $env:ProgramFiles 'WSL\wsl.exe'))) {
        if ($msix) { Write-OK "WSL runtime package: $($msix.Name) $($msix.Version)" }
    } else {
        Write-Warn 'only the inbox stub is present: the WSL runtime is not installed.'
        if ($Mode -eq 'repair') {
            Write-Step 'wsl --update'
            $r = Invoke-Native wsl @('--update') 900 'unicode'
            Write-Info $r.Text
        }
    }
    $deflt = Invoke-Native wsl @('--set-default-version', '2') 60 'unicode'
    if ($deflt.Exit -eq 0) {
        Write-OK 'default WSL version is 2.'
    } else {
        Write-Info $deflt.Text
    }
    $distros = Invoke-Native wsl @('--list', '--verbose') 30 'unicode'
    if ($distros.Exit -eq 0) {
        Write-OK 'at least one WSL distribution is installed.'
    } else {
        Write-Warn 'no WSL distribution yet: the docker-desktop distro is created by the first Docker Desktop start.'
    }
}

# ---------------------------------------------------------------------------
Write-Head '6. Docker engine'
$dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
if (-not $dockerCmd) {
    Write-Bad 'docker.exe is not on PATH: Docker Desktop is not installed.'
    $lastBootText | Out-File -FilePath $statePath -Encoding ascii -NoNewline
    exit (Get-ExitCode 1)
}
Write-Info "docker CLI: $($dockerCmd.Source)"

$info = Invoke-Native docker @('info', '--format', '{{.ServerVersion}}') 20 'utf8'
if ($info.Exit -eq 0 -and $info.Text) {
    Write-OK "docker daemon answered: server version $($info.Text)"
    Write-Host ''
    Write-Host 'Host is ready. Next:' -ForegroundColor Green
    Write-Host '  make docker-build' -ForegroundColor Green
    Write-Host '  powershell -ExecutionPolicy Bypass -File hack/kind-e2e.ps1 -Stage cluster,image,load,deploy,verify' -ForegroundColor Green
    $lastBootText | Out-File -FilePath $statePath -Encoding ascii -NoNewline
    exit (Get-ExitCode 0)
}

if ($info.Exit -eq 124) {
    Write-Bad 'docker info HUNG and was abandoned after 20s: the named pipe exists but nothing answers.'
    Write-Info 'That is a half-initialized Docker Desktop, not a broken CLI.'
} else {
    Write-Bad 'docker daemon is not reachable.'
    if ($info.Text) { Write-Info ("first line: " + ($info.Text -split "`n")[0].Trim()) }
}

# Docker Desktop must NOT be started from an elevated shell. Its per-user data
# (settings-store.json, the WSL distro, the named pipes) belongs to the normal
# user token, and starting it elevated produces a second, differently-owned set
# of those artefacts -- which then looks like "the engine started but the CLI
# cannot reach it". So this step is opt-in and refuses when elevated.
$ddRoot = Join-Path $env:LOCALAPPDATA 'Programs\DockerDesktop\Docker Desktop.exe'
if ($StartDocker) {
    if ($elevated) {
        Write-Warn '-StartDocker ignored: this shell is elevated. Start Docker Desktop'
        Write-Info 'from the Start menu instead (see the reasoning in this script).'
    } elseif (-not (Test-Path $ddRoot)) {
        Write-Bad "Docker Desktop.exe not found at $ddRoot"
    } else {
        Write-Step 'starting Docker Desktop and waiting for the engine'
        Start-Process -FilePath $ddRoot | Out-Null
        $deadline = (Get-Date).AddSeconds($DockerTimeoutSeconds)
        $ready = $false
        while ((Get-Date) -lt $deadline) {
            $probe = Invoke-Native docker @('info', '--format', '{{.ServerVersion}}') 20 'utf8'
            if ($probe.Exit -eq 0 -and $probe.Text) {
                Write-OK "engine up: server version $($probe.Text)"
                $ready = $true
                break
            }
            Start-Sleep -Seconds 5
        }
        if ($ready) {
            $lastBootText | Out-File -FilePath $statePath -Encoding ascii -NoNewline
            exit (Get-ExitCode 0)
        }
        Write-Bad "engine did not answer within $DockerTimeoutSeconds seconds."
        Write-Info 'Open Docker Desktop and read its own status line; the first start also'
        Write-Info 'creates the docker-desktop distro, which can take a few minutes.'
    }
} else {
    Write-Host ''
    Write-Host 'Last step (NOT done automatically; needs the normal, non-elevated desktop):' -ForegroundColor Yellow
    Write-Host '  Start Docker Desktop once and wait until it says "Docker Desktop is running".' -ForegroundColor Yellow
    Write-Host '  Its first start creates the docker-desktop distro and registers com.docker.service.' -ForegroundColor Yellow
    Write-Host '  Then re-run: powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1' -ForegroundColor Yellow
    Write-Host '  (or add -StartDocker and run this from a NON-elevated shell to have it wait for you)' -ForegroundColor Yellow
}

$lastBootText | Out-File -FilePath $statePath -Encoding ascii -NoNewline
exit (Get-ExitCode 1)
