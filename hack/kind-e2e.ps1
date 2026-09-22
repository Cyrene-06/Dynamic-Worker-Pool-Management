# hack/kind-e2e.ps1 -- end-to-end run of the control plane on a local kind cluster.
#
# WHY THIS EXISTS ALONGSIDE `make kind-e2e`:
#   The Makefile targets are the canonical, CI-facing entry point. This script is
#   the same sequence for Windows, where `make` is usually not installed -- and
#   "the commands exist in a Makefile you cannot run" is exactly how a milestone
#   ends up with every box unticked.
#
# WHY IT IS ASCII-ONLY: see hack/verify.ps1. PS 5.1 reads a BOM-less .ps1 as
# ANSI, so a non-ASCII literal breaks at PARSE time and the script exits -1 with
# zero output. Project docs are Chinese; every .ps1 in this repo is not.
#
# WHAT IT CLOSES (docs/10, M1 exit criteria):
#   - "sandbox-operator runs AS A CONTAINER on kind". The judged subject is the
#     Pod's ServiceAccount, never the developer's kubeconfig: the latter is
#     cluster-admin on kind and hides every RBAC gap (docs/10 R17). The verify
#     stage therefore audits permissions as that ServiceAccount, and greps the
#     operator's own log for "forbidden" -- the one place a missing permission
#     actually shows up.
#   - leader election switching. The verify stage reads the leader Lease,
#     deletes the holder, and waits for a different holder. "2 replicas are
#     configured" is not evidence that the handoff works.
#
# STAGE ORDER IS FIXED, whatever order you pass them in: cluster -> image ->
# load -> deploy -> verify. These are genuinely ordered steps, not independent
# tasks, and letting the caller reorder them would only produce confusing
# failures.
#
# USAGE:
#   # full run from nothing
#   powershell -ExecutionPolicy Bypass -File hack/kind-e2e.ps1 -Stage cluster,image,load,deploy,verify
#   # the common case: images already loaded, re-deploy and re-check
#   powershell -ExecutionPolicy Bypass -File hack/kind-e2e.ps1
#
# PREREQUISITES (checked in the preflight, not assumed):
#   Docker Desktop's Linux engine must answer. If it does not, run
#   hack/docker-doctor.ps1 (diagnose) and hack/host-ready.ps1 (repair) first --
#   this script refuses to start rather than produce a pile of confusing errors.
#
# EXIT CODES: 0 = every stage and check passed. 1 = something failed (the failing
# item is printed). The script never leaves a half-applied deployment on purpose:
# stages are independent, so re-running it is safe.

param(
    [ValidateSet('cluster', 'image', 'load', 'deploy', 'verify')]
    [string[]]$Stage = @('deploy', 'verify'),

    # Must match hack/kind-config.yaml's `name`. The mismatch is checked below
    # instead of documented, because a wrong cluster name produces
    # "cluster not found" from `kind load` while `kubectl` happily keeps talking
    # to whatever context is current -- a very confusing pair of symptoms.
    [string]$ClusterName = 'sandbox-dev',

    # Same defaults as the Makefile (IMAGE_REGISTRY / IMAGE_TAG). Keep in sync:
    # the manifests under config/ hardcode these names, so a change here without
    # a change there produces ImagePullBackOff, which reads like "the image was
    # never built".
    [string]$Registry = 'ghcr.io/cyrene-06',
    [string]$ImageTag = 'dev',

    # Defaults to $env:GOPROXY when set, else the public proxy -- the Makefile
    # does the same with `GOPROXY ?=`. In a network where proxy.golang.org is
    # unreachable the build does not fail, it just sits there.
    [string]$Goproxy = '',

    [string]$OperatorNamespace = 'sandbox-system',
    [string]$SandboxNamespace = 'sandbox-pool',

    [switch]$SkipRbacAudit,
    [switch]$SkipLeaderCheck,

    [int]$RolloutTimeoutSeconds = 180,
    [int]$LeaderSwitchTimeoutSeconds = 90
)

. (Join-Path $PSScriptRoot 'host-common.ps1')

if (-not $Goproxy) {
    if ($env:GOPROXY) { $Goproxy = $env:GOPROXY } else { $Goproxy = 'https://proxy.golang.org,direct' }
}

$repoRoot = Split-Path $PSScriptRoot -Parent
$kubeconfig = $env:KUBECONFIG
Push-Location $repoRoot

# ---------------------------------------------------------------------------
# helpers

# kubectl wrapper that reports the whole command line on failure -- "exit 1" on
# its own has cost this project more time than any single bug.
# Both wrappers report the WHOLE command line on failure. "exit 1" on its own has
# cost this project more time than any single bug. Note they do not count the
# failure themselves: Write-Bad already owns the counter, and counting twice
# would make the summary line nonsense.
function Invoke-Kubectl {
    param([string[]]$ArgList, [int]$TimeoutSeconds = 60, [switch]$AllowFailure)
    $r = Invoke-Native 'kubectl' $ArgList $TimeoutSeconds 'utf8'
    if ($r.Exit -ne 0 -and -not $AllowFailure) {
        Write-Bad ("kubectl " + ($ArgList -join ' ') + " failed (exit $($r.Exit))")
        if ($r.Text) { Write-Info $r.Text }
    }
    return $r
}

function Invoke-Docker {
    param([string[]]$ArgList, [int]$TimeoutSeconds = 60, [switch]$AllowFailure)
    $r = Invoke-Native 'docker' $ArgList $TimeoutSeconds 'utf8'
    if ($r.Exit -ne 0 -and -not $AllowFailure) {
        Write-Bad ("docker " + ($ArgList -join ' ') + " failed (exit $($r.Exit))")
        if ($r.Text) { Write-Info $r.Text }
    }
    return $r
}

# `kubectl auth can-i` prints yes/no on the LAST non-empty line and returns exit
# 1 when the answer is "no" -- so the exit code cannot distinguish "denied" from
# "the cluster is unreachable". Only the text can, which is why every audit row
# below looks at the line and not at $LASTEXITCODE.
function Get-AnswerLine {
    param([string]$Text)
    $lines = @($Text -split "`n" | Where-Object { $_.Trim() })
    if ($lines.Count -eq 0) { return '' }
    return $lines[-1].Trim()
}

# Convenience wrapper for the audit rows.
function Test-CanI {
    param([string]$Verb, [string]$Resource, [string]$Namespace, [string]$As)
    $args = @('auth', 'can-i', $Verb, $Resource)
    if ($Namespace) { $args += @('-n', $Namespace) }
    $args += @("--as=$As")
    $r = Invoke-Native 'kubectl' $args 60 'utf8'
    return @{ Allowed = ((Get-AnswerLine $r.Text) -eq 'yes'); Text = $r.Text }
}

function Wait-Until {
    param([scriptblock]$Condition, [int]$TimeoutSeconds, [int]$IntervalSeconds = 3)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if (& $Condition) { return $true }
        Start-Sleep -Seconds $IntervalSeconds
    }
    return $false
}

function Get-LeaseHolder {
    $r = Invoke-Native 'kubectl' @('-n', $OperatorNamespace, 'get', 'lease',
        'sandbox-operator.sandbox.example.com', '-o', 'jsonpath={.spec.holderIdentity}') 30 'utf8'
    if ($r.Exit -ne 0) { return $null }
    return $r.Text.Trim()
}

# ---------------------------------------------------------------------------
Write-Head 'Preflight'
$orderedStages = @('cluster', 'image', 'load', 'deploy', 'verify') | Where-Object { $Stage -contains $_ }
Write-Info ("stages            : " + ($orderedStages -join ' -> '))
Write-Info ("repo root         : $repoRoot")
Write-Info ("cluster name      : $ClusterName")
Write-Info ("images            : $Registry/dwp-{operator,gateway,sweeper}:$ImageTag")
Write-Info ("GOPROXY           : $Goproxy")
Write-Info ("kubeconfig        : " + $(if ($kubeconfig) { $kubeconfig } else { '(default)' }))

$needDocker = ($orderedStages -contains 'image') -or ($orderedStages -contains 'load')
$needKind = ($orderedStages -contains 'cluster') -or ($orderedStages -contains 'load')

if ($orderedStages -contains 'cluster' -or $orderedStages -contains 'load') {
    $cfgPath = Join-Path $PSScriptRoot 'kind-config.yaml'
    if (Test-Path $cfgPath) {
        $m = Select-String -Path $cfgPath -Pattern '^name:\s*(\S+)' | Select-Object -First 1
        if ($m) {
            $cfgName = $m.Matches[0].Groups[1].Value
            if ($cfgName -ne $ClusterName) {
                Write-Bad "cluster name mismatch: hack/kind-config.yaml says '$cfgName', -ClusterName is '$ClusterName'."
                Write-Info 'kind create would use the config file name, while kind load would look for the other one.'
                Pop-Location
                exit 1
            }
            Write-OK "cluster name matches hack/kind-config.yaml ('$cfgName')."
        }
    }
}

foreach ($exe in @('kubectl')) {
    if (-not (Get-Command $exe -ErrorAction SilentlyContinue)) {
        Write-Bad "$exe not found on PATH."
        Pop-Location
        exit 1
    }
}
if ($needKind -and -not (Get-Command kind -ErrorAction SilentlyContinue)) {
    Write-Bad 'kind not found on PATH (needed for the cluster/load stages).'
    Pop-Location
    exit 1
}
if ($needDocker) {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Write-Bad 'docker not found on PATH.'
        Pop-Location
        exit 1
    }
    # Fail here, loudly, rather than letting three builds each time out: an
    # unreachable Linux engine is the single most likely reason this script
    # cannot run at all on a fresh Windows machine.
    $probe = Invoke-Native 'docker' @('info', '--format', '{{.ServerVersion}}') 20 'utf8'
    if ($probe.Exit -ne 0 -or -not $probe.Text) {
        Write-Bad 'the docker daemon is not reachable; the image/load stages cannot run.'
        Write-Info 'diagnose: powershell -ExecutionPolicy Bypass -File hack/docker-doctor.ps1'
        Write-Info 'repair  : powershell -ExecutionPolicy Bypass -File hack/host-ready.ps1 -Mode repair'
        Pop-Location
        exit 1
    }
    Write-OK "docker daemon reachable (server $($probe.Text))."
}

# ---------------------------------------------------------------------------
if ($orderedStages -contains 'cluster') {
    Write-Head 'Stage 1: kind cluster'
    $clusters = Invoke-Native 'kind' @('get', 'clusters') 60 'utf8'
    # @() is not decoration: a pipeline that matches nothing yields $null, and
    # $null.Count is not 0 -- it is a trap that turns "no clusters" into a script
    # terminating on a property access.
    $exists = @($clusters.Text -split "`n" | Where-Object { $_.Trim() -eq $ClusterName }).Count -gt 0
    if ($exists) {
        Write-OK "cluster '$ClusterName' already exists (kind create skipped)."
    } else {
        Write-Step "creating cluster '$ClusterName' from hack/kind-config.yaml"
        $r = Invoke-Native 'kind' @('create', 'cluster', '--config', (Join-Path $PSScriptRoot 'kind-config.yaml')) 600 'utf8'
        if ($r.Exit -eq 0) { Write-OK 'cluster created.' } else {
            Write-Bad "kind create cluster failed (exit $($r.Exit))"
            Write-Info $r.Text
        }
    }
}

# ---------------------------------------------------------------------------
if ($orderedStages -contains 'image') {
    Write-Head 'Stage 2: build the three images'
    # One Dockerfile, three binaries (--build-arg BINARY). Intent: upgrading the
    # Go version stays a one-line change; see the Dockerfile header.
    $builds = @(
        @{ Binary = 'operator'; Image = "$Registry/dwp-operator:$ImageTag" },
        @{ Binary = 'gateway'; Image = "$Registry/dwp-gateway:$ImageTag" },
        @{ Binary = 'sweeper'; Image = "$Registry/dwp-sweeper:$ImageTag" }
    )
    foreach ($b in $builds) {
        Write-Step "building $($b.Image)"
        $r = Invoke-Docker @('build',
            '--build-arg', "VERSION=$ImageTag",
            '--build-arg', "GOPROXY=$Goproxy",
            '--build-arg', "BINARY=$($b.Binary)",
            '-t', $b.Image, '.') 1800
        if ($r.Exit -eq 0) { Write-OK "$($b.Image) built." }
    }
}

# ---------------------------------------------------------------------------
if ($orderedStages -contains 'load') {
    Write-Head 'Stage 3: load the images into the cluster'
    # This is REQUIRED, not an optimisation. kind "nodes" are containers: they
    # have their own image store and cannot see the host daemon's cache. Skipping
    # it produces ImagePullBackOff, which reads like "the image was never built".
    $images = @("$Registry/dwp-operator:$ImageTag", "$Registry/dwp-gateway:$ImageTag", "$Registry/dwp-sweeper:$ImageTag")
    $args = @('load', 'docker-image') + $images + @('--name', $ClusterName)
    $r = Invoke-Native 'kind' $args 900 'utf8'
    if ($r.Exit -eq 0) { Write-OK 'images loaded.' } else {
        Write-Bad "kind load failed (exit $($r.Exit))"
        Write-Info $r.Text
    }
}

# ---------------------------------------------------------------------------
if ($orderedStages -contains 'deploy') {
    Write-Head 'Stage 4: deploy the control plane'
    # Order matters twice over:
    #   config/samples creates the sandbox-pool / sandbox-system namespaces, and
    #   config/rbac contains a namespaced Role *in sandbox-pool* -- applying rbac
    #   first would fail for a reason that looks like a kustomize problem;
    #   config/isolation carries "which isolation levels exist in this cluster",
    #   and the controller would otherwise start on its built-in defaults and
    #   converge happily with the wrong facts.
    foreach ($k in @('config/crd', 'config/samples', 'config/rbac', 'config/isolation', 'config/manager')) {
        $r = Invoke-Kubectl @('apply', '-k', $k) 120
        if ($r.Exit -eq 0) { Write-OK "applied $k" }
    }
    Write-Step "waiting for the operator deployment (timeout ${RolloutTimeoutSeconds}s)"
    $r = Invoke-Kubectl @('-n', $OperatorNamespace, 'rollout', 'status',
        'deployment/sandbox-operator', "--timeout=$($RolloutTimeoutSeconds)s") ($RolloutTimeoutSeconds + 30)
    if ($r.Exit -eq 0) { Write-OK 'rollout complete.' }
}

# ---------------------------------------------------------------------------
if ($orderedStages -contains 'verify') {
    Write-Head 'Stage 5: verify'

    # -- 5.1 CRDs established ------------------------------------------------
    foreach ($crd in @('agentsandboxes.sandbox.example.com', 'sandboxpools.sandbox.example.com', 'sandboxtemplates.sandbox.example.com')) {
        $r = Invoke-Kubectl @('wait', '--for=condition=Established', "crd/$crd", '--timeout=60s') 90
        if ($r.Exit -eq 0) { Write-OK "CRD established: $crd" }
    }

    # -- 5.2 the operator runs as its own ServiceAccount ----------------------
    # The M1 criterion is explicit that the subject must be the Pod's SA. Using
    # the developer's kubeconfig instead (cluster-admin on kind) hides every RBAC
    # gap, which is how the missing PVC permission survived until R17.
    $saName = (Invoke-Kubectl @('-n', $OperatorNamespace, 'get', 'pods', '-l', 'app=sandbox-operator',
        '-o', 'jsonpath={.items[0].spec.serviceAccountName}') 30).Text
    if ($saName -eq 'sandbox-operator') {
        Write-OK "running as ServiceAccount '$saName' (not the developer's kubeconfig)."
    } else {
        Write-Bad "unexpected ServiceAccount on the operator Pod: '$saName'."
    }
    $replicas = Invoke-Kubectl @('-n', $OperatorNamespace, 'get', 'deployment', 'sandbox-operator',
        '-o', 'jsonpath={.status.replicas}') 30
    $readyReplicas = Invoke-Kubectl @('-n', $OperatorNamespace, 'get', 'deployment', 'sandbox-operator',
        '-o', 'jsonpath={.status.readyReplicas}') 30
    if ($readyReplicas.Text -eq $replicas.Text -and $replicas.Text -ne '') {
        Write-OK "all $($replicas.Text) replicas are ready."
    } else {
        Write-Bad "replicas not all ready: $($readyReplicas.Text)/$($replicas.Text)."
    }

    # -- 5.3 no RBAC failures in the operator's own log -----------------------
    # Cheapest true test of "the permissions are enough": whatever the controller
    # is missing shows up here as "forbidden", and nothing else does. Deliberately
    # not part of the audit below, which only proves what the RBAC objects say.
    $logs = Invoke-Kubectl @('-n', $OperatorNamespace, 'logs', 'deployment/sandbox-operator', '--tail=200') 60 -AllowFailure
    $forbidden = @($logs.Text -split "`n" | Where-Object { $_ -match 'forbidden|cannot (list|get|watch|create|delete|patch|update)' })
    if ($forbidden.Count -eq 0) {
        Write-OK 'no "forbidden" in the operator log (last 200 lines).'
    } else {
        Write-Bad "the operator log mentions permission errors ($($forbidden.Count) line(s)):"
        $forbidden | Select-Object -First 5 | ForEach-Object { Write-Info $_.Trim() }
    }

    # -- 5.4 leader election handoff -----------------------------------------
    if ($SkipLeaderCheck) {
        Write-Warn 'leader election check skipped (-SkipLeaderCheck).'
    } elseif ($replicas.Text -ne '2') {
        Write-Warn "leader election check skipped: needs 2 replicas, found '$($replicas.Text)'."
    } else {
        $holder = Get-LeaseHolder
        if (-not $holder) {
            Write-Bad 'could not read the leader Lease sandbox-operator.sandbox.example.com.'
            Write-Info 'If the LeaderElectionID in cmd/operator/main.go changed, update this script too.'
        } else {
            Write-Info "leader holder: $holder"
            $holderPod = $holder -replace '_.*$', ''
            Write-Step "deleting the leader Pod '$holderPod' and waiting for a handoff"
            Invoke-Kubectl @('-n', $OperatorNamespace, 'delete', 'pod', $holderPod, '--wait=false') 60 | Out-Null
            $switched = Wait-Until -TimeoutSeconds $LeaderSwitchTimeoutSeconds -Condition {
                $new = Get-LeaseHolder
                return ($new -and $new -ne $holder)
            }
            if ($switched) {
                Write-OK "handoff complete: the Lease now belongs to $(Get-LeaseHolder)."
            } else {
                Write-Bad "no handoff within ${LeaderSwitchTimeoutSeconds}s (lease expiry is ~15s)."
                Write-Info 'A stuck handoff usually means the replacement Pod is not running.'
            }
        }
    }

    # -- 5.5 permission audit, as the operator's ServiceAccount ---------------
    # The "expected no" rows are the valuable half: they are the assertions that
    # keep a future PR from quietly widening the control plane's blast radius
    # (a cluster-wide PVC delete, or exec into a sandbox, would both pass a
    # "does the operator work" test).
    if ($SkipRbacAudit) {
        Write-Warn 'RBAC audit skipped (-SkipRbacAudit).'
    } else {
        $sa = "system:serviceaccount:$OperatorNamespace:sandbox-operator"
        Write-Step "auditing permissions as $sa"
        $expectYes = @(
            @{ V = 'list';   R = 'pods';                                    N = $SandboxNamespace },
            @{ V = 'create'; R = 'leases.coordination.k8s.io';              N = $SandboxNamespace },
            @{ V = 'patch';  R = 'agentsandboxes/status';                   N = $SandboxNamespace },
            @{ V = 'list';   R = 'persistentvolumeclaims';                  N = $SandboxNamespace },
            @{ V = 'delete'; R = 'persistentvolumeclaims';                  N = $SandboxNamespace },
            @{ V = 'list';   R = 'nodes';                                   N = '' },
            @{ V = 'list';   R = 'runtimeclasses.node.k8s.io';              N = '' }
        )
        foreach ($c in $expectYes) {
            # Built as a variable rather than an inline subexpression: nesting a
            # quoted string inside $(if ...) inside a quoted string is legal but
            # reads as a puzzle, and the next editor will "simplify" it wrongly.
            $nsSuffix = ''
            if ($c.N) { $nsSuffix = " -n $($c.N)" }
            if ((Test-CanI -Verb $c.V -Resource $c.R -Namespace $c.N -As $sa).Allowed) {
                Write-OK "can $($c.V) $($c.R)$nsSuffix"
            } else {
                Write-Bad "CANNOT $($c.V) $($c.R)$nsSuffix -- expected yes."
            }
        }
        $expectNo = @(
            @{ V = 'delete'; R = 'persistentvolumeclaims'; N = 'default';           Why = 'the PVC permission must stay namespaced (docs/10 R17)' },
            @{ V = 'list';   R = 'secrets';                N = '';                  Why = 'the control plane never needs credentials (config/rbac/rbac.yaml)' },
            @{ V = 'create'; R = 'pods/exec';              N = $SandboxNamespace;   Why = 'exec into a sandbox destroys the isolation boundary' }
        )
        foreach ($c in $expectNo) {
            $nsSuffix = ''
            if ($c.N) { $nsSuffix = " -n $($c.N)" }
            if ((Test-CanI -Verb $c.V -Resource $c.R -Namespace $c.N -As $sa).Allowed) {
                Write-Bad "UNEXPECTEDLY ALLOWED: $($c.V) $($c.R)$nsSuffix -- $($c.Why)"
            } else {
                Write-OK "correctly denied: $($c.V) $($c.R)$nsSuffix"
            }
        }
        # Impersonation is what makes the audit above possible. If the SUBJECT
        # could impersonate, every "yes" above became a false positive -- RBAC
        # has no notion of "how did you get here". Checked last so the reader
        # sees the rows first. (Impersonate lives in the control plane's denied
        # set for a reason: see config/rbac/rbac.yaml.)
        $imp = Invoke-Native 'kubectl' @('auth', 'can-i', 'impersonate', 'serviceaccounts', "--as=$sa") 30 'utf8'
        if ((Get-AnswerLine $imp.Text) -eq 'yes') {
            Write-Bad 'the audit subject CAN impersonate: the rows above cannot prove anything.'
        } else {
            Write-OK 'the subject cannot impersonate, so the audit rows above are meaningful.'
        }
    }
}

# ---------------------------------------------------------------------------
Write-Head 'Result'
Pop-Location
if ($script:failed -gt 0) {
    Write-Host "$($script:failed) check(s) FAILED." -ForegroundColor Red
    exit 1
}
if ($script:warned -gt 0) {
    Write-Host "all checks passed, with $($script:warned) warning(s)." -ForegroundColor Yellow
    exit 0
}
Write-Host 'all checks passed.' -ForegroundColor Green
exit 0
