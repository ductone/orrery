#!/usr/bin/env python3
"""Capability bands, cost/capability Pareto frontier and tier comparison.

    python3 -I research/modelbench/cluster.py     # reads capability.json, writes clusters.json + clusters.md

Points are routable (model, effort) pairs of orrery models released 2025-06 or
later. Bands: 1-D Jenks natural breaks on theta, computed on well-placed
points only (se <= SE_MAX and the model has >= MIN_CODING coding cells); other
points are assigned to a band by their theta and flagged. Read-only on the
orrery catalog (~/.orrery/catalog/catalog-ramp.json) and internal/model/models.go.
"""
import json
import math
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
CAP = os.path.join(HERE, "capability.json")
OUT_JSON = os.path.join(HERE, "clusters.json")
OUT_MD = os.path.join(HERE, "clusters.md")
CATALOG = os.path.expanduser("~/.orrery/catalog/catalog-ramp.json")
MODELS_GO = os.path.join(REPO, "internal", "model", "models.go")

SE_MAX = 0.5        # points less certain than this do not shape the bands or the frontier
MIN_CODING = 2      # models with fewer coding-benchmark cells are "too sparse to place"
KNEE = 0.15         # best-value effort: cheapest routable effort within KNEE theta of the model's best
GVF_TARGET = 0.85
EFFORTS = ["none", "minimal", "low", "medium", "high", "xhigh", "max", "unspecified"]


def jenks(values, k):
    """Exact 1-D natural breaks by dynamic programming. Returns class upper bounds."""
    v = sorted(values)
    n = len(v)
    pre = [0.0]
    pre2 = [0.0]
    for x in v:
        pre.append(pre[-1] + x)
        pre2.append(pre2[-1] + x * x)

    def ssd(i, j):  # v[i:j]
        s = pre[j] - pre[i]
        return pre2[j] - pre2[i] - s * s / (j - i)

    INF = float("inf")
    cost = [[INF] * (n + 1) for _ in range(k + 1)]
    arg = [[0] * (n + 1) for _ in range(k + 1)]
    cost[0][0] = 0.0
    for c in range(1, k + 1):
        for j in range(c, n + 1):
            best, bi = INF, c - 1
            for i in range(c - 1, j):
                val = cost[c - 1][i] + ssd(i, j)
                if val < best:
                    best, bi = val, i
            cost[c][j], arg[c][j] = best, bi
    cuts = []
    j = n
    for c in range(k, 0, -1):
        i = arg[c][j]
        cuts.append((i, j))
        j = i
    cuts.reverse()
    total = ssd(0, n)
    gvf = 1 - cost[k][n] / total if total else 1.0
    return [(v[i], v[j - 1]) for i, j in cuts], gvf


def ncdf(x):
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def load_tiers():
    tiers = {}  # route -> tier
    try:
        cat = json.load(open(CATALOG))
        for m in cat.get("models", []):
            tiers[m["ID"]] = m.get("Tier")
    except OSError:
        pass
    try:
        src = open(MODELS_GO).read()
        for name, tier in re.findall(r'Name:\s*"([^"]+)"[^\n]*?Tier:\s*(\w+)', src):
            tiers.setdefault("models.go:" + name, tier.lower())
    except OSError:
        pass
    return tiers


def route_tiers(routes, tiers):
    out = {}
    for r in routes:
        if r in tiers:
            out[r] = tiers[r]
        name = r.rsplit("/", 1)[-1]
        if "models.go:" + name in tiers and not r.startswith("ramp/"):
            out[r] = tiers["models.go:" + name]
    return out


def fmt_e(e):
    return e


def main():
    cap = json.load(open(CAP))
    tiers = load_tiers()

    pts = []
    for m in cap["models"]:
        if not m["focus"]:
            continue
        rte = m["route_efforts"]
        for p in m["points"]:
            if rte and not p["routable"]:
                continue
            if not rte and p["effort"] != "unspecified" and any(q["effort"] == "unspecified" for q in m["points"]):
                continue
            sparse = m["n_coding_cells"] < MIN_CODING
            pts.append({
                "model": m["id"], "effort": p["effort"], "label": f'{m["id"]}@{p["effort"]}',
                "theta": p["theta"], "se": p["se"], "status": p["status"],
                "log_cost": p["log_cost"], "log_cost_se": p["log_cost_se"], "cost_usd_per_task": p["cost_usd_per_task"],
                "cost_source": p["cost_source"], "sparse_model": sparse,
                "well_placed": (not sparse) and p["se"] <= SE_MAX,
            })

    # ---- bands
    core = [p["theta"] for p in pts if p["well_placed"]]
    fits = {}
    for k in range(2, 7):
        fits[k] = jenks(core, k)
    k = next((k for k in range(2, 7) if fits[k][1] >= GVF_TARGET), 6)
    k = max(k, 3)
    classes, gvf = fits[k]
    # band boundaries: midpoints between adjacent classes; band 1 = highest
    bounds = [(classes[i][1] + classes[i + 1][0]) / 2 for i in range(len(classes) - 1)]
    edges = [-math.inf] + bounds + [math.inf]   # ascending
    nb = len(classes)

    def band_of(t):
        for i in range(nb):
            if edges[i] <= t < edges[i + 1]:
                return nb - i   # 1 = top
        return 1

    def tier_of_band(b):
        return "frontier" if b == 1 else ("tiny" if b == nb else "efficient")

    for p in pts:
        p["band"] = band_of(p["theta"])
        i = nb - p["band"]
        lo, hi = edges[i], edges[i + 1]
        p["band_prob"] = round(ncdf((hi - p["theta"]) / p["se"]) - ncdf((lo - p["theta"]) / p["se"]), 3)

    bands = []
    for b in range(1, nb + 1):
        i = nb - b
        mem = sorted([p for p in pts if p["band"] == b], key=lambda p: -p["theta"])
        bands.append({"band": b, "tier_equivalent": tier_of_band(b),
                      "theta_min": None if edges[i] == -math.inf else round(edges[i], 3),
                      "theta_max": None if edges[i + 1] == math.inf else round(edges[i + 1], 3),
                      "members": [p["label"] for p in mem]})

    # ---- Pareto frontier (maximize theta, minimize log cost) on well-placed points
    cand = [p for p in pts if p["well_placed"] and p["log_cost"] is not None]
    for p in cand:
        dom = [q for q in cand if q is not p and q["theta"] >= p["theta"] and q["log_cost"] <= p["log_cost"]
               and (q["theta"] > p["theta"] or q["log_cost"] < p["log_cost"])]
        p["dominated_by"] = None
        p["dominated_robust"] = False
        if dom:
            best = min(dom, key=lambda q: q["log_cost"])
            p["dominated_by"] = best["label"]
            # robust: some dominator is better by > 1 combined se in theta, or cheaper by > 1 combined cost se
            p["dominated_robust"] = any(
                (q["theta"] - p["theta"]) > math.hypot(q["se"], p["se"]) or
                (p["log_cost"] - q["log_cost"]) > math.hypot(q["log_cost_se"] or 0, p["log_cost_se"] or 0)
                for q in dom)
    frontier = sorted([p for p in cand if p["dominated_by"] is None], key=lambda p: p["log_cost"])

    # ---- per-route recommendation
    capm = {m["id"]: m for m in cap["models"]}
    routes = []
    for mid in sorted({p["model"] for p in pts}):
        mp = sorted([p for p in pts if p["model"] == mid], key=lambda p: EFFORTS.index(p["effort"]))
        best_t = max(p["theta"] for p in mp)
        # prefer well-placed points; fall back to all when a model has none
        pool = [p for p in mp if p["se"] <= SE_MAX] or mp
        best_t = max(p["theta"] for p in pool)
        costed = [p for p in pool if p["log_cost"] is not None]
        ok = [p for p in costed if p["theta"] >= best_t - KNEE] or pool
        bv = min(ok, key=lambda p: (p["log_cost"] if p["log_cost"] is not None else 0, EFFORTS.index(p["effort"])))
        placed = [p for p in mp if p["se"] <= SE_MAX]
        ceiling = max(placed or mp, key=lambda p: p["theta"])
        # comparable only if some placed effort is within one level of the highest routable effort
        top = max(EFFORTS.index(p["effort"]) for p in mp)
        if placed and max(EFFORTS.index(p["effort"]) for p in placed) < top - 1:
            placed = []
        rt = route_tiers(capm[mid]["orrery_routes"], tiers)
        labels = sorted(set(rt.values()))
        sparse = mp[0]["sparse_model"]
        rec_band = bv["band"]
        disagree = []
        if labels and not sparse and placed:
            want = tier_of_band(ceiling["band"])
            for t in labels:
                if t != want:
                    disagree.append(t)
        routes.append({
            "model": mid, "routes": capm[mid]["orrery_routes"], "current_tiers": rt,
            "n_coding_cells": capm[mid]["n_coding_cells"], "sparse": sparse,
            "efforts": [{"effort": p["effort"], "theta": p["theta"], "se": p["se"], "band": p["band"],
                         "band_prob": p["band_prob"], "log_cost": p["log_cost"], "cost_source": p["cost_source"],
                         "status": p["status"], "dominated_by": p.get("dominated_by")} for p in mp],
            "best_value_effort": bv["effort"], "recommended_band": rec_band,
            "ceiling_effort": ceiling["effort"], "ceiling_band": ceiling["band"],
            "tier_from_bands": tier_of_band(ceiling["band"]),
            "placed": bool(placed) and not sparse,
            "disagrees_with": disagree,
        })

    out = {
        "generated_from": "cluster.py",
        "settings": {"se_max": SE_MAX, "min_coding_cells": MIN_CODING, "knee": KNEE, "gvf_target": GVF_TARGET},
        "jenks": {"k": nb, "gvf": round(gvf, 4), "gvf_by_k": {str(kk): round(v[1], 4) for kk, v in fits.items()}},
        "bands": bands,
        "pareto_frontier": [p["label"] for p in frontier],
        "points": sorted(pts, key=lambda p: -p["theta"]),
        "routes": routes,
    }
    with open(OUT_JSON, "w") as f:
        json.dump(out, f, indent=1)
        f.write("\n")
    write_md(out, cap)


def write_md(out, cap):
    L = []
    w = L.append
    nb = out["jenks"]["k"]
    w("# modelbench clusters\n")
    w("Generated by `cluster.py` from `capability.json`. Points are routable `model@effort` pairs of orrery "
      "models released 2025-06 or later. theta is the latent coding ability (sd 1 over all 400+ fitted models, "
      "mean 0); cost is ln(USD per CursorBench-equivalent task).\n")
    v = cap["validation"]["mean"]
    w(f"Held-out RMSE (z units, 15% of focus coding cells, 3 splits): model **{v['rmse_model']:.2f}**, "
      f"benchmark mean {v['rmse_bench_mean']:.2f}, benchmark + model mean {v['rmse_bench_plus_model_mean']:.2f}.\n")
    w(f"Bands: Jenks natural breaks, k={nb} (GVF {out['jenks']['gvf']:.2f}; by k: "
      + ", ".join(f"{k}:{g}" for k, g in out["jenks"]["gvf_by_k"].items()) + "), fit on well-placed points "
      f"(se <= {out['settings']['se_max']}, model with >= {out['settings']['min_coding_cells']} coding cells).\n")
    w("## Bands\n")
    w("| band | tier equiv | theta range | well-placed members |")
    w("|---|---|---|---|")
    pmap = {p["label"]: p for p in out["points"]}
    for b in out["bands"]:
        lo = "-inf" if b["theta_min"] is None else f'{b["theta_min"]:.2f}'
        hi = "inf" if b["theta_max"] is None else f'{b["theta_max"]:.2f}'
        mem = [m for m in b["members"] if pmap[m]["well_placed"]]
        w(f'| {b["band"]} | {b["tier_equivalent"]} | [{lo}, {hi}) | {", ".join(mem)} |')
    w("")
    w("## Routes\n")
    w("Best value = cheapest routable effort within "
      f"{out['settings']['knee']} theta of the model's best routable effort. Ceiling = band of the best routable effort. "
      "cost src: M measured, E1 estimated from the model's own measured efforts, E2 list price x effort multiplier. "
      "status: m measured at that effort, x extrapolated from other efforts (+-0.6 prior), "
      "s = model too sparse to place.\n")
    w("| model | current tier | theta +- se by effort (band) | cost idx by effort (src) | best value | rec band | ceiling | disagree |")
    w("|---|---|---|---|---|---|---|---|")
    src = lambda s: "M" if s == "measured" else ("E1" if "other efforts" in s else ("E2" if "list price" in s else "?"))
    for r in sorted(out["routes"], key=lambda r: (r["recommended_band"], -max(e["theta"] for e in r["efforts"]))):
        th = "; ".join(f'{e["effort"]} {e["theta"]:.2f}+-{e["se"]:.2f} ({e["band"]}{"" if e["status"] == "measured" else "x"})'
                       for e in r["efforts"])
        cs = "; ".join(f'{e["effort"]} {e["log_cost"]:.2f} {src(e["cost_source"])}' if e["log_cost"] is not None
                       else f'{e["effort"]} n/a' for e in r["efforts"])
        tiers = ", ".join(sorted(set(r["current_tiers"].values()))) or "-"
        flag = " (s)" if r["sparse"] else ""
        w(f'| {r["model"]}{flag} | {tiers} | {th} | {cs} | {r["best_value_effort"]} | {r["recommended_band"]} | '
          f'{r["ceiling_effort"]}->{r["ceiling_band"]} | {", ".join(r["disagrees_with"]) or ""} |')
    w("")
    w("## Pareto frontier (capability vs cost, well-placed points)\n")
    w("| point | theta +- se | cost idx | $/task equiv | cost src |")
    w("|---|---|---|---|---|")
    for lab in out["pareto_frontier"]:
        p = pmap[lab]
        w(f'| {lab} | {p["theta"]:.2f} +- {p["se"]:.2f} | {p["log_cost"]:.2f} | {p["cost_usd_per_task"]:.2f} | {src(p["cost_source"])} |')
    w("")
    dom = [p for p in out["points"] if p.get("dominated_by") and p.get("dominated_robust")]
    w(f"Robustly dominated (a dominator is better by > 1 combined se in theta or cost): {len(dom)} of "
      f"{sum(1 for p in out['points'] if 'dominated_by' in p)} costed well-placed points. "
      "Full list with dominators in `clusters.json` (`points[].dominated_by`).\n")
    w("## Disagreements with current tiers\n")
    w("Tier from bands: band 1 = frontier, last band = tiny, others = efficient; compared at the ceiling effort. "
      "Sparse models are not compared.\n")
    w("| model | current | bands say | ceiling (well-placed) theta | note |")
    w("|---|---|---|---|---|")
    for r in out["routes"]:
        if r["disagrees_with"]:
            c = next(e for e in r["efforts"] if e["effort"] == r["ceiling_effort"])
            cur = ", ".join(f"{k}={v}" for k, v in sorted(r["current_tiers"].items()))
            w(f'| {r["model"]} | {cur} | {r["tier_from_bands"]} | {r["ceiling_effort"]} {c["theta"]:.2f} +- {c["se"]:.2f} | band prob {c["band_prob"]:.2f} |')
    w("")
    sp = [r["model"] for r in out["routes"] if r["sparse"]]
    up = [r["model"] for r in out["routes"] if not r["sparse"] and not r["placed"]]
    w("## Too sparse to place\n")
    w(f"Fewer than {out['settings']['min_coding_cells']} coding-benchmark cells; theta rests on general benchmarks "
      "and the release-date prior: " + (", ".join(sp) if sp else "none") + ".\n")
    w("No well-placed routable effort near the top of the route's effort range (data only at unspecified, "
      f"non-routable or low efforts; the rest is extrapolated with se > {out['settings']['se_max']}); not compared with tiers: "
      + (", ".join(up) if up else "none") + ".\n")
    w("## Caveats\n")
    w("- E2 costs (list price x global effort multiplier) ignore per-model verbosity; their log sd is ~1.1, so "
      "frontier membership resting on E2 cost (gpt-6-luna, gpt-6.1-sol, gpt-oss-120b) is provisional.\n"
      "- Effort deviations for unmeasured efforts default to the global effort curve (+-0.6 prior sd).\n"
      "- Ceiling ignores `max`, which most routes do not expose.\n"
      "- No speed axis: only FrontierSWE reports duration.\n")
    with open(OUT_MD, "w") as f:
        f.write("\n".join(L))


if __name__ == "__main__":
    main()
