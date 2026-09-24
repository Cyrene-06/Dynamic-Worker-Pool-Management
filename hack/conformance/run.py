#!/usr/bin/env python3
"""E2 Kata conformance runner. Run on the KVM node; writes evidence, never invented results."""
import argparse
import concurrent.futures
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone


def utc():
    return datetime.now(timezone.utc).isoformat()


def cmd(*args, input_text=None, timeout=120, check=True):
    p = subprocess.run(args, input=input_text, text=True, capture_output=True, timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"{' '.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
    return p


def kubectl(*args, input_text=None, timeout=120, check=True):
    return cmd("kubectl", *args, input_text=input_text, timeout=timeout, check=check)


def kjson(*args):
    return json.loads(kubectl(*args, "-o", "json").stdout)


def pod(name, image, runtime, namespace, command=None, labels=None, volumes=None, mounts=None, memory="256Mi", node=None):
    c = {"name": "app", "image": image, "command": command or ["sh", "-c", "echo conformance-ready; sleep 3600"],
         "imagePullPolicy": "Always",
         "resources": {"requests": {"cpu": "100m", "memory": "64Mi"}, "limits": {"memory": memory}},
         "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                             "runAsNonRoot": True, "runAsUser": 65532, "capabilities": {"drop": ["ALL"]}}}
    if mounts:
        c["volumeMounts"] = mounts
    spec = {"runtimeClassName": runtime, "restartPolicy": "Never", "automountServiceAccountToken": False,
            "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}}, "containers": [c]}
    if node:
        spec["nodeSelector"] = {"kubernetes.io/hostname": node}
    if volumes:
        spec["volumes"] = volumes
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": namespace,
            "labels": {"app": "kata-conformance", **(labels or {})}}, "spec": spec}


def apply(obj):
    kubectl("apply", "-f", "-", input_text=json.dumps(obj))


def getpod(ns, name):
    return kjson("-n", ns, "get", "pod", name)


def waitpod(ns, name, timeout=180, terminal=False):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        p = getpod(ns, name)
        phase = p.get("status", {}).get("phase")
        if terminal and phase in ("Succeeded", "Failed"):
            return p
        if not terminal and any(c.get("type") == "Ready" and c.get("status") == "True"
                                for c in p.get("status", {}).get("conditions", [])):
            return p
        time.sleep(.25)
    raise TimeoutError(f"pod {ns}/{name} did not become {'terminal' if terminal else 'Ready'} in {timeout}s")


def cleanup(ns, name, kind="pod"):
    kubectl("-n", ns, "delete", kind, name, "--ignore-not-found", "--wait=false", check=False)


def snapshot():
    procs = []
    inotify = 0
    for entry in pathlib.Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            for fd in (entry / "fd").iterdir():
                try:
                    if "inotify" in os.readlink(fd):
                        inotify += 1
                except OSError:
                    pass
            comm = (entry / "comm").read_text().strip()
            if comm in ("firecracker", "virtiofsd", "cloud-hypervisor"):
                rss_pages = int((entry / "statm").read_text().split()[1])
                cg = (entry / "cgroup").read_text()
                match = re.search(r"pod([0-9a-f]{8}[-_][0-9a-f_-]{20,})", cg)
                procs.append({"pid": int(entry.name), "name": comm, "rss_bytes": rss_pages * os.sysconf("SC_PAGE_SIZE"),
                              "pod_uid": match.group(1).replace("_", "-") if match else None})
        except (OSError, ValueError, IndexError):
            pass
    def read(path):
        try:
            return pathlib.Path(path).read_text().strip()
        except OSError:
            return None
    return {"vmm_processes": procs, "vmm_rss_bytes": sum(p["rss_bytes"] for p in procs),
            "host_pid_count": sum(1 for x in pathlib.Path("/proc").iterdir() if x.name.isdigit()),
            "cpu_load_1m": float(read("/proc/loadavg").split()[0]),
            "inotify_instances": inotify,
            "pid_max": read("/proc/sys/kernel/pid_max"),
            "mem_available_kib": next((int(x.split()[1]) for x in read("/proc/meminfo").splitlines()
                                       if x.startswith("MemAvailable:")), None),
            "inotify_max_instances": read("/proc/sys/fs/inotify/max_user_instances"),
            "inotify_max_watches": read("/proc/sys/fs/inotify/max_user_watches")}


def stages(p, started):
    conditions = {c["type"]: c.get("lastTransitionTime") for c in p.get("status", {}).get("conditions", [])}
    def seconds(ts):
        if not ts:
            return None
        return (datetime.fromisoformat(ts.replace("Z", "+00:00")) - started).total_seconds()
    scheduled = seconds(conditions.get("PodScheduled"))
    initialized = seconds(conditions.get("Initialized"))
    containers_ready = seconds(conditions.get("ContainersReady"))
    ready = seconds(conditions.get("Ready"))
    return {"scheduled_s": scheduled,
            "initialized_s": initialized - scheduled if initialized is not None and scheduled is not None else None,
            "containers_ready_s": containers_ready - initialized if containers_ready is not None and initialized is not None else None,
            "ready_s": ready - containers_ready if ready is not None and containers_ready is not None else None}


def create_and_time(a, name, image=None, runtime=None, labels=None):
    started = datetime.now(timezone.utc)
    apply(pod(name, image or a.image, runtime or a.runtime, a.namespace, labels=labels, node=a.node))
    p = waitpod(a.namespace, name, a.timeout)
    events = kjson("-n", a.namespace, "get", "events", "--field-selector", f"involvedObject.uid={p['metadata']['uid']}")
    pulls = [{"reason": e.get("reason"), "message": e.get("message"), "at": e.get("lastTimestamp") or e.get("eventTime")}
             for e in events.get("items", []) if e.get("reason") in ("Pulling", "Pulled")]
    return {"pod": name, "started_at": started.isoformat(), "elapsed_s":
            (datetime.now(timezone.utc) - started).total_seconds(), "stages": stages(p, started),
            "node": p.get("spec", {}).get("nodeName"), "pod_ip": p.get("status", {}).get("podIP"),
            "pull_events": pulls}


def t1(a):
    rows = []
    for density in a.densities:
        names = [f"kata-t1-{a.run_id}-{density}-{i}" for i in range(density)]
        try:
            with concurrent.futures.ThreadPoolExecutor(max_workers=min(density, 16)) as pool:
                futures = [pool.submit(create_and_time, a, name) for name in names]
                samples = [f.result() for f in futures]
            rows.append({"density": density, "samples": samples, "host": snapshot()})
        finally:
            for name in names:
                cleanup(a.namespace, name)
            kubectl("-n", a.namespace, "wait", "--for=delete", "pod", "-l", "app=kata-conformance",
                    "--timeout=180s", check=False)
    return {"rows": rows, "node_capacity": kjson("get", "node", a.node).get("status", {}).get("allocatable", {})}


def t2(a):
    name = f"kata-t2-{a.run_id}"
    try:
        sample = create_and_time(a, name)
        kernel = kubectl("-n", a.namespace, "exec", name, "--", "uname", "-r").stdout.strip()
        logs = kubectl("-n", a.namespace, "logs", name).stdout
        if "conformance-ready" not in logs or not kernel or kernel == os.uname().release:
            raise RuntimeError("exec/logs or guest kernel isolation check failed")
        return {"kernel_guest": kernel, "kernel_host": os.uname().release, "logs": logs, "sample": sample}
    finally:
        cleanup(a.namespace, name)


def t3(a):
    name = f"kata-t3-{a.run_id}"
    volumes = [{"name": "scratch", "emptyDir": {}}]
    mounts = [{"name": "scratch", "mountPath": "/work"}]
    if a.csi_claim:
        volumes.append({"name": "csi", "persistentVolumeClaim": {"claimName": a.csi_claim}})
        mounts.append({"name": "csi", "mountPath": "/csi"})
    try:
        apply(pod(name, a.image, a.runtime, a.namespace, volumes=volumes, mounts=mounts, node=a.node))
        waitpod(a.namespace, name, a.timeout)
        results = {}
        for path in (["/work", "/csi"] if a.csi_claim else ["/work"]):
            script = f"dd if=/dev/zero of={path}/data bs=1M count=16 && sha256sum {path}/data"
            start = time.monotonic()
            out = kubectl("-n", a.namespace, "exec", name, "--", "sh", "-c", script).stdout
            results[path] = {"sha256": re.search(r"[0-9a-f]{64}", out).group(0), "write_s": time.monotonic() - start}
        return results
    finally:
        cleanup(a.namespace, name)


def t4(a):
    if not a.lazy_image or not a.lazy_snapshotter:
        raise Skip("--lazy-image and --lazy-snapshotter are required")
    plugins = cmd("ctr", "plugins", "ls").stdout
    if a.lazy_snapshotter not in plugins:
        raise RuntimeError(f"snapshotter {a.lazy_snapshotter} not present in containerd plugins")
    config = cmd("containerd", "config", "dump").stdout
    if f'snapshotter = "{a.lazy_snapshotter}"' not in config:
        raise RuntimeError(f"containerd is not configured to use {a.lazy_snapshotter}")
    name = f"kata-t4-{a.run_id}"
    try:
        return create_and_time(a, name, image=a.lazy_image)
    finally:
        cleanup(a.namespace, name)


def t5(a):
    if not a.policy_pod or not a.allowed_url or not a.blocked_url:
        raise Skip("--policy-pod, --allowed-url and --blocked-url are required")
    urls = {"allowed": a.allowed_url, "blocked": a.blocked_url,
            "api_server": "https://kubernetes.default.svc", "metadata": "http://169.254.169.254"}
    results = {}
    for key, url in urls.items():
        p = kubectl("-n", a.namespace, "exec", a.policy_pod, "--", "curl", "-ksS", "--connect-timeout", "3",
                    "--max-time", "6", url, timeout=15, check=False)
        results[key] = {"exit_code": p.returncode, "stderr": p.stderr[-300:]}
    if results["allowed"]["exit_code"] or any(results[k]["exit_code"] == 0 for k in ("blocked", "api_server", "metadata")):
        raise RuntimeError(f"network policy violated: {results}")
    if a.gateway_pod and a.outside_pod and a.policy_port:
        ip = getpod(a.namespace, a.policy_pod).get("status", {}).get("podIP")
        if not ip:
            raise RuntimeError("policy pod has no IP")
        ingress = {}
        for key, source in (("gateway", a.gateway_pod), ("outside", a.outside_pod)):
            p = kubectl("-n", a.probe_namespace, "exec", source, "--", "curl", "-sS", "--connect-timeout", "3",
                        "--max-time", "6", f"http://{ip}:{a.policy_port}", timeout=15, check=False)
            ingress[key] = {"exit_code": p.returncode, "stderr": p.stderr[-300:]}
        if ingress["gateway"]["exit_code"] or ingress["outside"]["exit_code"] == 0:
            raise RuntimeError(f"gateway-only ingress violated: {ingress}")
        results["ingress"] = ingress
    else:
        results["ingress"] = "not exercised; provide --gateway-pod, --outside-pod and --policy-port"
    return results


def t6(a):
    if not a.oom_image:
        raise Skip("--oom-image with python3 is required")
    name = f"kata-t6-{a.run_id}"
    evidence = {}
    try:
        obj = pod(name, a.oom_image, a.runtime, a.namespace,
                  command=["python3", "-c", "a=bytearray(256*1024*1024); print(len(a))"], memory="64Mi", node=a.node)
        apply(obj)
        p = waitpod(a.namespace, name, a.timeout, terminal=True)
        statuses = p.get("status", {}).get("containerStatuses", [])
        reason = statuses[0].get("state", {}).get("terminated", {}).get("reason") if statuses else None
        if reason != "OOMKilled":
            raise RuntimeError(f"expected OOMKilled, got {reason}")
        evidence["oom_reason"] = reason
    finally:
        cleanup(a.namespace, name)
    if not a.exercise_eviction:
        evidence["eviction"] = "not exercised; use --exercise-eviction on a disposable E2 node"
        return evidence
    evict_name = f"kata-t6-evict-{a.run_id}"
    try:
        obj = pod(evict_name, a.image, a.runtime, a.namespace,
                  command=["sh", "-c", "dd if=/dev/zero of=/work/fill bs=1M count=128; sleep 3600"],
                  volumes=[{"name": "scratch", "emptyDir": {}}],
                  mounts=[{"name": "scratch", "mountPath": "/work"}], node=a.node)
        obj["spec"]["containers"][0]["resources"]["requests"]["ephemeral-storage"] = "1Mi"
        obj["spec"]["containers"][0]["resources"]["limits"]["ephemeral-storage"] = "16Mi"
        apply(obj)
        p = waitpod(a.namespace, evict_name, max(a.timeout, 300), terminal=True)
        reason = p.get("status", {}).get("reason")
        if reason != "Evicted":
            raise RuntimeError(f"expected Evicted, got {reason}: {p.get('status', {}).get('message', '')}")
        evidence["eviction"] = reason
        return evidence
    finally:
        cleanup(a.namespace, evict_name)


def t7(a):
    name = f"kata-t7-{a.run_id}"
    try:
        create_and_time(a, name)
        patch = {"spec": {"containers": [{"name": "app", "resources": {"requests": {"cpu": "200m", "memory": "64Mi"},
                                                     "limits": {"memory": "256Mi"}}}]}}
        p = kubectl("-n", a.namespace, "patch", "pod", name, "--subresource=resize", "--type=strategic",
                    "-p", json.dumps(patch), check=False)
        return {"supported": p.returncode == 0, "response": (p.stdout or p.stderr)[-1000:]}
    finally:
        cleanup(a.namespace, name)


def t8(a):
    marker = a.output / "t8-before.json"
    boot_id = pathlib.Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    if not marker.exists():
        marker.write_text(json.dumps({"boot_id": boot_id, "before": snapshot(), "time": utc()}, indent=2))
        raise Skip("T8 pre-reboot snapshot saved; reboot E2 node, then rerun with same --output")
    before = json.loads(marker.read_text())
    if before["boot_id"] == boot_id:
        raise Skip("node has not rebooted since T8 pre-reboot snapshot")
    after = snapshot()
    active = {x["metadata"]["uid"] for x in kjson("get", "pods", "--all-namespaces")["items"]}
    orphans = [p for p in after["vmm_processes"] if p["pod_uid"] and p["pod_uid"] not in active]
    if orphans:
        raise RuntimeError(f"orphan VMM processes after reboot: {orphans}")
    return {"before": before, "after": after, "boot_id_changed": True,
            "orphan_vmm_count": len(orphans)}


def t9(a):
    if not a.upgrade_runtime:
        raise Skip("--upgrade-runtime is required")
    old_name, new_name = f"kata-t9-old-{a.run_id}", f"kata-t9-new-{a.run_id}"
    try:
        old = create_and_time(a, old_name)
        new = create_and_time(a, new_name, runtime=a.upgrade_runtime)
        cleanup(a.namespace, new_name)
        rollback_name = f"kata-t9-rollback-{a.run_id}"
        rollback = create_and_time(a, rollback_name)
        cleanup(a.namespace, rollback_name)
        return {"old": old, "new": new, "rollback": rollback,
                "scope": "RuntimeClass switch/rollback; node binary upgrade must be recorded separately"}
    finally:
        cleanup(a.namespace, old_name)
        cleanup(a.namespace, new_name)


class Skip(Exception):
    pass


CASES = {"T1": t1, "T2": t2, "T3": t3, "T4": t4, "T5": t5, "T6": t6, "T7": t7, "T8": t8, "T9": t9}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--namespace", default="sandbox-pool")
    parser.add_argument("--node", required=True)
    parser.add_argument("--runtime", default="kata-fc")
    parser.add_argument("--image", default="registry.k8s.io/busybox:1.36")
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--densities", type=lambda s: [int(x) for x in s.split(",")], default=[1, 5, 10, 20, 40])
    parser.add_argument("--cases", default=",".join(CASES))
    parser.add_argument("--lazy-image")
    parser.add_argument("--lazy-snapshotter")
    parser.add_argument("--csi-claim")
    parser.add_argument("--policy-pod")
    parser.add_argument("--allowed-url")
    parser.add_argument("--blocked-url")
    parser.add_argument("--gateway-pod")
    parser.add_argument("--outside-pod")
    parser.add_argument("--probe-namespace", default="sandbox-system")
    parser.add_argument("--policy-port", type=int)
    parser.add_argument("--oom-image")
    parser.add_argument("--exercise-eviction", action="store_true", help="opt in to bounded ephemeral-storage pressure on disposable E2 node")
    parser.add_argument("--upgrade-runtime")
    a = parser.parse_args()
    a.output.mkdir(parents=True, exist_ok=True)
    a.run_id = uuid.uuid4().hex[:8]
    selected = [x.strip() for x in a.cases.split(",")]
    if any(x not in CASES for x in selected):
        parser.error("unknown case in --cases")
    results = {"run_id": a.run_id, "at": utc(), "node": a.node, "runtime": a.runtime, "cases": {}}
    for key in selected:
        try:
            evidence = CASES[key](a)
            status = "PARTIAL" if (key == "T9" or (key == "T6" and not a.exercise_eviction) or (key == "T3" and not a.csi_claim)
                                   or (key == "T5" and not (a.gateway_pod and a.outside_pod and a.policy_port))) else "PASS"
            if key == "T7" and not evidence["supported"]:
                status = "INCOMPATIBLE"
            results["cases"][key] = {"status": status, "evidence": evidence}
        except Skip as e:
            results["cases"][key] = {"status": "SKIP", "reason": str(e)}
        except Exception as e:
            results["cases"][key] = {"status": "FAIL", "reason": str(e)}
        (a.output / "results.json").write_text(json.dumps(results, ensure_ascii=False, indent=2))
        print(f"{key}: {results['cases'][key]['status']}")
    return 1 if any(x["status"] == "FAIL" for x in results["cases"].values()) else 0


if __name__ == "__main__":
    sys.exit(main())
