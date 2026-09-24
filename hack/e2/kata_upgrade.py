#!/usr/bin/env python3
"""Rehearse a versioned Kata RuntimeClass switch and forced rollback on E2."""
import argparse
import json
import pathlib
import subprocess
import time
import uuid
from datetime import datetime, timezone


def now():
    return datetime.now(timezone.utc).isoformat()


def kubectl(*args, data=None, check=True):
    p = subprocess.run(["kubectl", *args], input=data, text=True, capture_output=True)
    if check and p.returncode:
        raise RuntimeError(f"kubectl {' '.join(args)}: {p.stderr.strip()}")
    return p


def js(*args):
    return json.loads(kubectl(*args, "-o", "json").stdout)


def patch_pool(name, runtime, drain, cold_disabled, reason="Kata RuntimeClass upgrade rehearsal"):
    patch = {"spec": {"runtimeClassName": runtime,
                       "drain": {"enabled": drain, "reason": reason},
                       "degradation": {"disableColdPath": cold_disabled}}}
    kubectl("patch", "sandboxpool", name, "--type=merge", "-p", json.dumps(patch))


def wait_drained(name, timeout):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        status = js("get", "sandboxpool", name).get("status", {})
        if status.get("warm", 0) == 0 and status.get("draining", 0) == 0:
            return {"warm": 0, "draining": 0, "claimed": status.get("claimed", 0),
                    "waited_s": time.monotonic() - start}
        time.sleep(2)
    raise TimeoutError(f"pool {name} did not drain within {timeout}s")


def probe(pool, runtime, namespace, timeout):
    name = f"kata-upgrade-{uuid.uuid4().hex[:10]}"
    obj = {"apiVersion": "sandbox.example.com/v1alpha1", "kind": "AgentSandbox",
           "metadata": {"name": name, "namespace": namespace, "labels": {
               "sandbox.example.com/pool": pool["metadata"]["name"],
               "sandbox.example.com/tier": pool["spec"]["tier"],
               "sandbox.example.com/template": pool["spec"]["templateRef"]["name"],
               "sandbox.example.com/isolation": "kata-fc", "sandbox.example.com/claimed": "false",
               "sandbox.example.com/role": "sandbox"}},
           "spec": {"poolRef": {"name": pool["metadata"]["name"]},
                    "templateRef": pool["spec"]["templateRef"], "tier": pool["spec"]["tier"],
                    "isolation": "kata-fc", "runtimeClassName": pool["spec"].get("runtimeClassName", "")}}
    kubectl("apply", "-f", "-", data=json.dumps(obj))
    start = time.monotonic()
    try:
        while time.monotonic() - start < timeout:
            s = js("-n", namespace, "get", "agentsandbox", name)
            status = s.get("status", {})
            if status.get("runtimeClassName") == runtime and status.get("podName"):
                pod = js("-n", namespace, "get", "pod", status["podName"])
                if any(c.get("type") == "Ready" and c.get("status") == "True" for c in pod.get("status", {}).get("conditions", [])):
                    return {"sandbox": name, "pod": status["podName"], "runtime": status["runtimeClassName"],
                            "node": pod.get("spec", {}).get("nodeName"), "ready_after_s": time.monotonic() - start}
            time.sleep(2)
        raise TimeoutError(f"sandbox {name} did not become Ready with {runtime}")
    finally:
        kubectl("-n", namespace, "delete", "agentsandbox", name, "--ignore-not-found", "--wait=false", check=False)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--pool", required=True)
    ap.add_argument("--new-runtime", required=True)
    ap.add_argument("--namespace", default="sandbox-pool")
    ap.add_argument("--observe-seconds", type=int, default=60)
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--output", type=pathlib.Path, required=True)
    a = ap.parse_args()
    a.output.mkdir(parents=True, exist_ok=True)
    pool = js("get", "sandboxpool", a.pool)
    if pool["spec"]["isolation"] != "kata-fc":
        raise RuntimeError("selected pool is not kata-fc")
    original = pool["spec"]
    old_runtime = original.get("runtimeClassName") or "kata-fc"
    old_rc = js("get", "runtimeclass", old_runtime)
    new_rc = js("get", "runtimeclass", a.new_runtime)
    if old_rc["handler"] != new_rc["handler"]:
        raise RuntimeError("old and new RuntimeClass handlers differ")
    original_cold = original.get("degradation", {}).get("disableColdPath", False)
    original_drain = original.get("drain", {}).get("enabled", False)
    original_reason = original.get("drain", {}).get("reason", "")
    if original_drain:
        raise RuntimeError("pool is already draining; finish existing maintenance first")
    record = {"started_at": now(), "pool": a.pool, "old_runtime": old_runtime,
              "new_runtime": a.new_runtime, "steps": [], "status": "INCOMPLETE"}
    output = a.output / "kata-upgrade.json"
    try:
        record["steps"].append({"at": now(), "action": "old-runtime-probe", "result": probe(pool, old_runtime, a.namespace, a.timeout)})
        patch_pool(a.pool, original.get("runtimeClassName"), True, True)
        record["steps"].append({"at": now(), "action": "drain-old-stock", "result": wait_drained(a.pool, a.timeout)})
        patch_pool(a.pool, a.new_runtime, False, original_cold)
        record["steps"].append({"at": now(), "action": "switch-target-pool", "runtime": a.new_runtime})
        new_pool = js("get", "sandboxpool", a.pool)
        record["steps"].append({"at": now(), "action": "new-runtime-probe", "result": probe(new_pool, a.new_runtime, a.namespace, a.timeout)})
        time.sleep(a.observe_seconds)
        record["steps"].append({"at": now(), "action": "injected-failure", "reason": "exercise rollback after successful canary"})
        patch_pool(a.pool, a.new_runtime, True, True)
        record["steps"].append({"at": now(), "action": "drain-new-stock", "result": wait_drained(a.pool, a.timeout)})
        patch_pool(a.pool, original.get("runtimeClassName"), False, original_cold)
        rollback_pool = js("get", "sandboxpool", a.pool)
        record["steps"].append({"at": now(), "action": "rollback-probe", "result": probe(rollback_pool, old_runtime, a.namespace, a.timeout)})
        record["status"] = "PASS"
    except Exception as e:
        record["status"] = "FAIL"
        record["error"] = str(e)
        raise
    finally:
        # Drain any new-version inventory before restoring the old class.
        try:
            current = js("get", "sandboxpool", a.pool)
            if current["spec"].get("runtimeClassName") != original.get("runtimeClassName"):
                patch_pool(a.pool, current["spec"].get("runtimeClassName"), True, True)
                wait_drained(a.pool, a.timeout)
            patch_pool(a.pool, original.get("runtimeClassName"), original_drain, original_cold, original_reason)
        except Exception as restore_error:
            record["status"] = "FAIL"
            record["restore_error"] = str(restore_error)
        record["finished_at"] = now()
        output.write_text(json.dumps(record, ensure_ascii=False, indent=2))
        print(f"upgrade rehearsal evidence: {output}")


if __name__ == "__main__":
    main()
