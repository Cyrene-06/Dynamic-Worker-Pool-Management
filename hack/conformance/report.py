#!/usr/bin/env python3
"""Turn measured E2 results.json into a baseline report, CSV and SVG chart."""
import argparse
import csv
import json
import math
import pathlib
import statistics


def p95(values):
    values = sorted(x for x in values if x is not None)
    return values[math.ceil(.95 * len(values)) - 1] if values else None


def cpu_cores(quantity):
    q = str(quantity)
    return float(q[:-1]) / 1000 if q.endswith("m") else float(q)


def svg_chart(rows, path):
    width, height, left, bottom = 760, 440, 72, 380
    max_x = max(x["density"] for x in rows)
    max_y = max(x["p95_s"] for x in rows if x["p95_s"] is not None) * 1.1
    x = lambda d: left + (d / max_x) * (width - left - 30)
    y = lambda v: bottom - (v / max_y) * (bottom - 40)
    points = " ".join(f"{x(r['density']):.1f},{y(r['p95_s']):.1f}" for r in rows if r["p95_s"] is not None)
    circles = "".join(f'<circle cx="{x(r["density"]):.1f}" cy="{y(r["p95_s"]):.1f}" r="5" fill="#1769aa"/>'
                      for r in rows if r["p95_s"] is not None)
    labels = "".join(f'<text x="{x(r["density"]):.1f}" y="{bottom+25}" text-anchor="middle" font-size="12">{r["density"]}</text>' for r in rows)
    path.write_text(f'''<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">
<rect width="100%" height="100%" fill="white"/><text x="20" y="24" font-size="18">Kata cold-path P95 by node density</text>
<line x1="{left}" y1="40" x2="{left}" y2="{bottom}" stroke="#444"/><line x1="{left}" y1="{bottom}" x2="{width-30}" y2="{bottom}" stroke="#444"/>
<polyline points="{points}" fill="none" stroke="#1769aa" stroke-width="3"/>{circles}{labels}
<text x="{width//2}" y="{height-12}" text-anchor="middle" font-size="13">Sandboxes per node</text>
<text x="20" y="{bottom//2}" transform="rotate(-90 20 {bottom//2})" text-anchor="middle" font-size="13">P95 seconds</text></svg>''')


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("results", type=pathlib.Path)
    ap.add_argument("--output", type=pathlib.Path, required=True)
    ap.add_argument("--ipam-available", type=int, help="measured Cilium IPAM available IPs before test")
    a = ap.parse_args()
    data = json.loads(a.results.read_text())
    a.output.mkdir(parents=True, exist_ok=True)
    t1 = data.get("cases", {}).get("T1", {})
    rows = []
    if t1.get("status") == "PASS":
        for row in t1["evidence"]["rows"]:
            samples = row["samples"]
            stages = {key: p95([s["stages"].get(key) for s in samples]) for key in
                      ("scheduled_s", "initialized_s", "containers_ready_s", "ready_s")}
            rows.append({"density": row["density"], "count": len(samples),
                         "p95_s": p95([s["elapsed_s"] for s in samples]),
                         "p50_s": statistics.median(s["elapsed_s"] for s in samples),
                         "vmm_rss_bytes": row["host"]["vmm_rss_bytes"],
                         "host_pid_count": row["host"]["host_pid_count"],
                         "pid_max": row["host"].get("pid_max"),
                         "inotify_instances": row["host"].get("inotify_instances"),
                         "inotify_max_instances": row["host"].get("inotify_max_instances"),
                         "mem_available_kib": row["host"].get("mem_available_kib"),
                         "cpu_load_1m": row["host"]["cpu_load_1m"], **stages})
    with (a.output / "density.csv").open("w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=["density", "count", "p50_s", "p95_s", "scheduled_s",
                                 "initialized_s", "containers_ready_s", "ready_s", "vmm_rss_bytes",
                                 "host_pid_count", "pid_max", "inotify_instances", "inotify_max_instances",
                                 "mem_available_kib", "cpu_load_1m"])
        writer.writeheader()
        writer.writerows(rows)
    if rows:
        svg_chart(rows, a.output / "density-latency.svg")
    recommendation = "未给出：需要 T1 实测结果、Cilium IPAM 可用地址数以及 PID/inotify 消耗曲线。"
    if len(rows) >= 2 and a.ipam_available is not None and all(r["inotify_instances"] is not None for r in rows):
        baseline = rows[0]["p95_s"]
        knee = next((r["density"] for r in rows[1:] if r["p95_s"] > baseline * 1.2), rows[-1]["density"])
        caps = {"latency": max(1, math.floor(knee * .8)), "ipam": a.ipam_available}
        first, last = rows[0], rows[-1]
        delta = last["density"] - first["density"]
        for key, limit, label in (("host_pid_count", "pid_max", "pid"), ("inotify_instances", "inotify_max_instances", "inotify")):
            slope = (last[key] - first[key]) / delta
            if slope > 0 and first[limit] is not None:
                caps[label] = first["density"] + max(0, math.floor((int(first[limit]) - first[key]) / slope))
        rss_slope = (last["vmm_rss_bytes"] - first["vmm_rss_bytes"]) / delta
        if rss_slope > 0 and first["mem_available_kib"] is not None:
            caps["vmm_memory"] = first["density"] + max(0, math.floor(first["mem_available_kib"] * 1024 / rss_slope))
        cpu_limit = next((r["density"] for r in rows if r["cpu_load_1m"] >
                          cpu_cores(t1["evidence"]["node_capacity"].get("cpu", "0")) * .8), None)
        if cpu_limit:
            caps["cpu_load"] = max(1, math.floor(cpu_limit * .8))
        chosen = min(caps.values())
        cap_text = ", ".join(f"{k}={v}" for k, v in caps.items())
        recommendation = (f"建议候选上限 {chosen}；观测拐点 {knee}，约束计算：{cap_text}。"
                          "PID/inotify 与 VMM 内存上限是线性外推，需在更高密度再次实测；"
                          "如果最高采样密度仍未达到延迟拐点，这个值只是保守候选值。")
    lines = ["# M2 E2 启动延迟与密度基线", "", f"- 采样时间：{data.get('at', '未知')}",
             f"- 节点：{data.get('node', '未知')}", f"- RuntimeClass：{data.get('runtime', '未知')}",
             "- 数据来源：同目录 `results.json`，未填补缺失样本。", "", "## T1 分阶段启动延迟", ""]
    if not rows:
        lines.append("**未采集**：没有通过的 T1 实测数据，无法给出冷路径 P95。")
    else:
        lines += ["| 密度 | 样本数 | P50 总耗时 | P95 总耗时 | P95 调度 | P95 初始化 | P95 容器就绪 | VMM RSS |",
                  "|---:|---:|---:|---:|---:|---:|---:|---:|"]
        for r in rows:
            lines.append(f"| {r['density']} | {r['count']} | {r['p50_s']:.3f}s | {r['p95_s']:.3f}s | "
                         f"{r['scheduled_s']}s | {r['initialized_s']}s | {r['containers_ready_s']}s | {r['vmm_rss_bytes']} B |")
        lines += ["", "![密度与延迟曲线](density-latency.svg)", "",
                  f"- 低密度冷路径 P95：**{rows[0]['p95_s']:.3f}s**；目标 ≤ 2.5s。",
                  f"- 判定：{'达标' if rows[0]['p95_s'] <= 2.5 else '未达标，需优化或评估 kata-clh'}。"]
    lines += ["", "## 密度上限", "", recommendation, "", "## T1–T9 一致性结果", "",
              "| 项目 | 状态 | 备注 |", "|---|---|---|"]
    for i in range(1, 10):
        case = data.get("cases", {}).get(f"T{i}", {})
        lines.append(f"| T{i} | {case.get('status', '未执行')} | {case.get('reason', '')} |")
    lines += ["", "## 升级演练", "", "见 `kata-upgrade.json`；若文件不存在，升级与回滚尚未执行。", ""]
    (a.output / "baseline.md").write_text("\n".join(lines), encoding="utf-8")
    print(a.output / "baseline.md")


if __name__ == "__main__":
    main()
