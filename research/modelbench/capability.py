#!/usr/bin/env python3
"""Joint latent-ability and cost model over observations.jsonl.

    python3 -I research/modelbench/capability.py      # writes capability.json

Stdlib only, deterministic (fixed seeds). See README.md, "capability.py".

Model (per selected benchmark b, primary metric only):

    z_i = a_b + s_b * theta(m, e) + eps,   eps ~ N(0, sigma_b^2 / w_i)
    theta(m, e) = theta_m + g_e + u_{m,e}

z is the transformed score (logit for pct, log for minutes, linear for
elo/score/index), z-standardized per benchmark. g_e is a global effort offset
(g_high = 0), u_{m,e} a per-model deviation with a ridge prior N(0, TAU^2).
"unspecified" is its own effort level with its own estimated global offset
(it is NOT assumed equal to any level). theta_m has a ridge prior centred on a
release-date trend (empirical Bayes) so sparse models shrink toward models of
the same vintage. Fit by alternating (block-coordinate) ridge least squares.
Uncertainty: bootstrap over (model, effort, benchmark) cells plus the
conditional Laplace variance of each point.
"""
import json
import math
import os
import random
import sys
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
OBS = os.path.join(HERE, "observations.jsonl")
MODELS = os.path.join(HERE, "models.json")
OUT = os.path.join(HERE, "capability.json")

EFFORTS = ["none", "minimal", "low", "medium", "high", "xhigh", "max", "unspecified"]
ORD = {e: i for i, e in enumerate(EFFORTS[:-1])}

# (benchmark, metric) -> (transform, weight, group). One primary metric per benchmark.
BENCH = {
    ("cursorbench", "score"): ("logit", 1.0, "coding"),
    ("deepswe", "pass@1"): ("logit", 1.0, "coding"),          # not pass@4
    ("frontierswe", "score"): ("logit", 1.0, "coding"),       # not best@5
    ("frontiercode", "score"): ("logit", 1.0, "coding"),
    ("swe_bench_verified", "score"): ("logit", 1.0, "coding"),  # Epoch's run; swebench-site board skipped (same tasks)
    ("terminalbench", "score"): ("logit", 1.0, "coding"),
    ("aider_polyglot", "score"): ("logit", 1.0, "coding"),
    ("gso", "score"): ("logit", 1.0, "coding"),
    ("metr_time_horizon", "p50_horizon"): ("log", 1.0, "coding"),
    ("lmarena_webdev:overall", "rating"): ("linear", 1.0, "coding"),  # epoch webdev_arena skipped (same arena)
    ("lmarena_agent:overall", "score"): ("linear", 1.0, "coding"),
    ("weirdml", "score"): ("logit", 0.5, "coding"),
    ("scicode", "score"): ("logit", 0.5, "general"),
    ("epoch_eci", "eci"): ("linear", 0.5, "general"),
    ("gpqa_diamond", "score"): ("logit", 0.5, "general"),
    ("lmarena_text:overall", "rating"): ("linear", 0.5, "general"),
}

TAU = 0.6          # prior sd of per-model effort deviation u_{m,e}
SD_THETA = 1.0     # prior sd of theta_m around the date trend
SD_G = 1.5         # prior sd of global effort offsets
ITERS = 150
BOOT = 50
BOOT_ITERS = 40
SEED = 20261012
BLEND_IN, BLEND_OUT = 3.0, 1.0   # price blend: 3 input : 1 output tokens


def load():
    obs = []
    with open(OBS) as f:
        for line in f:
            obs.append(json.loads(line))
    models = {m["id"]: m for m in json.load(open(MODELS))["models"]}
    return obs, models


def year(d):
    if not d:
        return None
    y, mo = int(d[:4]), int(d[5:7])
    return y + (mo - 0.5) / 12.0


def solve(A, b):
    """Gaussian elimination with partial pivoting. Returns x, and A^-1 diag."""
    n = len(b)
    M = [row[:] + [b[i]] + [1.0 if j == i else 0.0 for j in range(n)] for i, row in enumerate(A)]
    for c in range(n):
        p = max(range(c, n), key=lambda r: abs(M[r][c]))
        M[c], M[p] = M[p], M[c]
        piv = M[c][c]
        for k in range(c, 2 * n + 1):
            M[c][k] /= piv
        for r in range(n):
            if r != c and M[r][c] != 0.0:
                f = M[r][c]
                for k in range(c, 2 * n + 1):
                    M[r][k] -= f * M[c][k]
    x = [M[i][n] for i in range(n)]
    inv = [[M[i][n + 1 + j] for j in range(n)] for i in range(n)]
    return x, inv


# ---------------------------------------------------------------- data

def build_cells(obs):
    """Select rows and transform. Returns list of cells:
    (model, effort, bench, z_raw_mean, weight) — replicates in one cell are averaged."""
    raw = defaultdict(list)
    for o in obs:
        key = (o["benchmark"], o["metric"])
        if key not in BENCH or not o["model"] or o["value"] is None:
            continue
        tr = BENCH[key][0]
        v = float(o["value"])
        if tr == "logit":
            p = min(max(v / 100.0, 0.005), 0.995)
            z = math.log(p / (1 - p))
        elif tr == "log":
            if v <= 0:
                continue
            z = math.log(v)
        else:
            z = v
        if o.get("higher_is_better") is False:
            z = -z
        raw[(o["model"], o["effort"], key[0])].append(z)
    # per-benchmark standardization
    byb = defaultdict(list)
    for (m, e, b), zs in raw.items():
        byb[b].append(sum(zs) / len(zs))
    norm = {}
    for b, vs in byb.items():
        mu = sum(vs) / len(vs)
        sd = math.sqrt(sum((v - mu) ** 2 for v in vs) / max(1, len(vs) - 1)) or 1.0
        norm[b] = (mu, sd)
    wmap = {k[0]: v[1] for k, v in BENCH.items()}
    cells = []
    for (m, e, b), zs in sorted(raw.items()):
        mu, sd = norm[b]
        z = (sum(zs) / len(zs) - mu) / sd
        # replicates (e.g. several Terminal-Bench harnesses) get a small precision bonus, capped
        w = wmap[b] * min(2.0, 1.0 + 0.25 * (len(zs) - 1))
        cells.append((m, e, b, z, w))
    return cells, norm


# ---------------------------------------------------------------- fit

class Fit:
    pass


def fit(cells, dates, init=None, iters=ITERS):
    models = sorted({c[0] for c in cells})
    benches = sorted({c[2] for c in cells})
    bym = defaultdict(list)
    byb = defaultdict(list)
    bye = defaultdict(list)
    for i, c in enumerate(cells):
        bym[c[0]].append(i)
        byb[c[2]].append(i)
        bye[c[1]].append(i)
    if init is None:
        theta = {m: 0.0 for m in models}
        u = {}
        g = {e: 0.0 for e in EFFORTS}
        g.update({"none": -1.0, "minimal": -0.7, "low": -0.4, "medium": -0.2, "xhigh": 0.1, "max": 0.15})
        a = {b: 0.0 for b in benches}
        s = {b: 1.0 for b in benches}
        sig = {b: 0.5 for b in benches}
        trend = (0.0, 0.0)
    else:
        theta = {m: init.theta.get(m, 0.0) for m in models}
        u = dict(init.u)
        g = dict(init.g)
        a = {b: init.a.get(b, 0.0) for b in benches}
        s = {b: init.s.get(b, 1.0) for b in benches}
        sig = {b: init.sig.get(b, 0.5) for b in benches}
        trend = init.trend

    def prior_mean(m):
        y = dates.get(m)
        return trend[0] + trend[1] * (y - 2025.5) if y is not None else trend[0]

    lap = {}
    for it in range(iters):
        # A: per-model (theta_m, u_{m,e})
        for m in models:
            idx = bym[m]
            effs = sorted({cells[i][1] for i in idx}, key=EFFORTS.index)
            k = 1 + len(effs)
            pos = {e: j + 1 for j, e in enumerate(effs)}
            A = [[0.0] * k for _ in range(k)]
            rhs = [0.0] * k
            A[0][0] += 1.0 / SD_THETA ** 2
            rhs[0] += prior_mean(m) / SD_THETA ** 2
            for e in effs:
                A[pos[e]][pos[e]] += 1.0 / TAU ** 2
            for i in idx:
                _, e, b, z, w = cells[i]
                ww = w / sig[b] ** 2
                sb = s[b]
                r = z - a[b] - sb * g[e]
                j = pos[e]
                A[0][0] += ww * sb * sb
                A[0][j] += ww * sb * sb
                A[j][0] += ww * sb * sb
                A[j][j] += ww * sb * sb
                rhs[0] += ww * sb * r
                rhs[j] += ww * sb * r
            x, inv = solve(A, rhs)
            theta[m] = x[0]
            for e in effs:
                u[(m, e)] = x[pos[e]]
            if it == iters - 1:
                lap[m] = (effs, inv)
        # B: global effort offsets (g_high = 0)
        for e in EFFORTS:
            if e == "high" or e not in bye:
                continue
            num = 0.0
            den = 1.0 / SD_G ** 2
            for i in bye[e]:
                m, _, b, z, w = cells[i]
                ww = w / sig[b] ** 2
                num += ww * s[b] * (z - a[b] - s[b] * (theta[m] + u[(m, e)]))
                den += ww * s[b] ** 2
            g[e] = num / den
        # C: benchmark location/scale/noise
        for b in benches:
            S = [[1.0 / 9.0, 0.0], [0.0, 1.0 / 2.25]]
            R = [0.0, 1.0 / 2.25]
            for i in byb[b]:
                m, e, _, z, w = cells[i]
                t = theta[m] + g[e] + u[(m, e)]
                S[0][0] += w
                S[0][1] += w * t
                S[1][0] += w * t
                S[1][1] += w * t * t
                R[0] += w * z
                R[1] += w * t * z
            (ab, sb), _ = solve(S, R)
            a[b], s[b] = ab, max(0.1, sb)
            ssr = 0.0
            wn = 0.0
            for i in byb[b]:
                m, e, _, z, w = cells[i]
                t = theta[m] + g[e] + u[(m, e)]
                ssr += w * (z - a[b] - s[b] * t) ** 2
                wn += w
            sig[b] = math.sqrt((ssr + 3 * 0.3 ** 2) / (wn + 3))
        # renormalize: theta_m mean 0 / sd 1 over all fitted models
        mu = sum(theta.values()) / len(theta)
        sd = math.sqrt(sum((t - mu) ** 2 for t in theta.values()) / len(theta)) or 1.0
        for m in models:
            theta[m] = (theta[m] - mu) / sd
        for key in u:
            u[key] /= sd
        for e in g:
            g[e] /= sd
        for b in benches:
            a[b] += s[b] * mu
            s[b] *= sd
        # empirical-Bayes date trend for the theta prior
        xs = [(dates[m], theta[m]) for m in models if dates.get(m) is not None and len(bym[m]) >= 3]
        if len(xs) > 10:
            mx = sum(x for x, _ in xs) / len(xs)
            my = sum(y for _, y in xs) / len(xs)
            sxx = sum((x - mx) ** 2 for x, _ in xs)
            sxy = sum((x - mx) * (y - my) for x, y in xs)
            beta = sxy / sxx if sxx else 0.0
            trend = (my + beta * (2025.5 - mx), beta)
    F = Fit()
    F.theta, F.u, F.g, F.a, F.s, F.sig, F.trend, F.lap = theta, u, g, a, s, sig, trend, lap
    F.models, F.benches = models, benches
    F.nobs = {m: len(bym[m]) for m in models}
    return F


def point(F, m, e):
    """theta(m, e); u is 0 (its prior mean) when the model has no data at e."""
    return F.theta.get(m, F.trend[0]) + F.g.get(e, 0.0) + F.u.get((m, e), 0.0)


def lap_var(F, m, e):
    if m not in F.lap:
        return SD_THETA ** 2 + TAU ** 2
    effs, inv = F.lap[m]
    if e in effs:
        j = effs.index(e) + 1
        return inv[0][0] + inv[j][j] + 2 * inv[0][j]
    return inv[0][0] + TAU ** 2


def predict(F, m, e, b):
    return F.a.get(b, 0.0) + F.s.get(b, 1.0) * point(F, m, e)


# ---------------------------------------------------------------- validation

def validate(cells, dates, focus, seeds=(1, 2, 3)):
    res = []
    for sd in seeds:
        rng = random.Random(SEED + sd)
        cand = [i for i, c in enumerate(cells) if c[0] in focus and BENCH_GROUP[c[2]] == "coding"]
        hold = set(rng.sample(cand, int(round(0.15 * len(cand)))))
        train = [c for i, c in enumerate(cells) if i not in hold]
        test = [cells[i] for i in sorted(hold)]
        F = fit(train, dates, iters=100)
        bmean = defaultdict(list)
        for c in train:
            bmean[c[2]].append(c[3])
        bmean = {b: sum(v) / len(v) for b, v in bmean.items()}
        # stronger baseline: benchmark mean + the model's mean residual on its other training cells
        mres = defaultdict(list)
        for c in train:
            mres[c[0]].append(c[3] - bmean[c[2]])
        se_m = se_b = se_mm = 0.0
        for m, e, b, z, w in test:
            se_m += (z - predict(F, m, e, b)) ** 2
            se_b += (z - bmean.get(b, 0.0)) ** 2
            off = sum(mres[m]) / len(mres[m]) if mres.get(m) else 0.0
            se_mm += (z - bmean.get(b, 0.0) - off) ** 2
        n = len(test)
        res.append({"seed": sd, "n_test": n, "rmse_model": math.sqrt(se_m / n), "rmse_bench_mean": math.sqrt(se_b / n),
                    "rmse_bench_plus_model_mean": math.sqrt(se_mm / n)})
    avg = {k: sum(r[k] for r in res) / len(res) for k in ("rmse_model", "rmse_bench_mean", "rmse_bench_plus_model_mean")}
    return {"splits": res, "mean": avg,
            "note": "z units (per-benchmark standardized transformed score); 15% of focus-model coding cells held out per split"}


# ---------------------------------------------------------------- cost

def blended_price(mdl):
    p = mdl.get("pricing") or {}
    if p.get("input") is None or p.get("output") is None:
        return None
    return (BLEND_IN * float(p["input"]) + BLEND_OUT * float(p["output"])) / (BLEND_IN + BLEND_OUT)


def cost_model(obs, models):
    """ln cost(b, m, e) = c_b + K_{m,e}; c_cursorbench = 0, so K is ln(USD per
    CursorBench-equivalent task). Then K_{m,e} - ln price_m = k0 + k_m + h_e,
    used to estimate K where no measurement exists."""
    rows = defaultdict(list)
    for o in obs:
        if (o["benchmark"], o["metric"]) in (("cursorbench", "score"), ("deepswe", "pass@1"), ("frontierswe", "score")) \
                and o["model"] and o["cost_usd"] and o["cost_usd"] > 0:
            rows[(o["benchmark"], o["model"], o["effort"])].append(math.log(o["cost_usd"]))
    data = [(b, m, e, sum(v) / len(v)) for (b, m, e), v in sorted(rows.items())]
    benches = sorted({d[0] for d in data})
    c = {b: 0.0 for b in benches}
    K = {}
    for _ in range(100):
        acc = defaultdict(list)
        for b, m, e, y in data:
            acc[(m, e)].append(y - c[b])
        K = {k: sum(v) / len(v) for k, v in acc.items()}
        for b in benches:
            if b == "cursorbench":
                continue
            rs = [y - K[(m, e)] for bb, m, e, y in data if bb == b]
            c[b] = sum(rs) / len(rs)
    resid = [y - c[b] - K[(m, e)] for b, m, e, y in data]
    meas_sd = math.sqrt(sum(r * r for r in resid) / max(1, len(resid) - len(K))) if resid else 0.3
    nK = defaultdict(int)
    for b, m, e, y in data:
        nK[(m, e)] += 1
    # decompose token usage: K - ln price = k0 + k_m + h_e
    pts = []
    for (m, e), v in K.items():
        p = blended_price(models.get(m, {}))
        if p and e in ORD:
            pts.append((m, e, v - math.log(p)))
    k0 = sum(p[2] for p in pts) / len(pts)
    km = defaultdict(float)
    h = {e: 0.0 for e in ORD}
    for _ in range(100):
        acc = defaultdict(list)
        for m, e, y in pts:
            acc[e].append(y - k0 - km[m])
        for e, v in acc.items():
            h[e] = sum(v) / (len(v) + 1.0)
        hh = h["high"]
        for e in acc:
            h[e] -= hh
        acc = defaultdict(list)
        for m, e, y in pts:
            acc[m].append(y - k0 - h[e])
        k0 = sum(sum(v) for v in acc.values()) / sum(len(v) for v in acc.values())
        km = defaultdict(float, {m: sum(y - k0 for y in v) / (len(v) + 1.0) for m, v in acc.items()})
    nmod = defaultdict(set)
    for m, e, _ in pts:
        nmod[e].add(m)
    observed_e = {e for e in nmod if len(nmod[e]) >= 3}  # a level needs >= 3 models to be trusted
    # monotone (non-decreasing) from low upward; levels below low, or without enough
    # data, are extrapolated one ordinal step at the low->medium slope
    for e in ("medium", "high", "xhigh", "max"):
        prev = EFFORTS[EFFORTS.index(e) - 1]
        h[e] = max(h[e], h[prev]) if e in observed_e else h[prev] + (h["medium"] - h["low"])
    lo_slope = h["medium"] - h["low"]
    if "minimal" not in observed_e:
        h["minimal"] = h["low"] - lo_slope
    if "none" not in observed_e:
        h["none"] = h["minimal"] - lo_slope
    h["unspecified"] = h["high"]
    res2 = [y - k0 - km[m] - h[e] for m, e, y in pts]
    tok_sd = math.sqrt(sum(r * r for r in res2) / max(1, len(res2)))
    km_sd = math.sqrt(sum(v * v for v in km.values()) / max(1, len(km)))
    return {"c_b": c, "K": K, "nK": nK, "meas_sd": meas_sd, "k0": k0, "km": dict(km), "h": h,
            "h_measured": sorted(observed_e, key=EFFORTS.index), "tok_sd": tok_sd, "km_sd": km_sd}


def cost_point(CM, models, m, e):
    if (m, e) in CM["K"]:
        return CM["K"][(m, e)], CM["meas_sd"] / math.sqrt(CM["nK"][(m, e)]), "measured"
    p = blended_price(models.get(m, {}))
    if not p:
        return None, None, "unknown"
    own = [(K, ee) for (mm, ee), K in CM["K"].items() if mm == m and ee in CM["h"]]
    if own:
        # anchor on the model's own measured efforts, shifted by the global effort multiplier
        est = sum(K + CM["h"].get(e, 0.0) - CM["h"][ee] for K, ee in own) / len(own)
        return est, CM["tok_sd"], "estimated (model measured at other efforts)"
    return math.log(p) + CM["k0"] + CM["h"].get(e, 0.0), math.sqrt(CM["tok_sd"] ** 2 + CM["km_sd"] ** 2), "estimated (list price x effort multiplier)"


# ---------------------------------------------------------------- main

BENCH_GROUP = {k[0]: v[2] for k, v in BENCH.items()}


def main():
    obs, models = load()
    cells, norm = build_cells(obs)
    dates = {m: year(models[m].get("release_date")) for m in models}
    for o in obs:  # fall back to release dates carried on observations
        rd = (o.get("extra") or {}).get("release_date")
        if o["model"] and dates.get(o["model"]) is None and rd:
            dates[o["model"]] = year(rd)
    focus = {m for m, v in models.items() if v.get("orrery_routes") and (v.get("release_date") or "") >= "2025-06"}
    print(f"cells={len(cells)} models={len({c[0] for c in cells})} focus={len(focus)}", file=sys.stderr)

    F = fit(cells, dates)
    print("fit done", file=sys.stderr)

    # bootstrap over cells
    rng = random.Random(SEED)
    boots = []
    for r in range(BOOT):
        sample = [cells[rng.randrange(len(cells))] for _ in range(len(cells))]
        boots.append(fit(sample, dates, init=F, iters=BOOT_ITERS))
    print("bootstrap done", file=sys.stderr)

    val = validate(cells, dates, focus)
    print("validation", val["mean"], file=sys.stderr)

    CM = cost_model(obs, models)

    # per-model coverage
    cov = defaultdict(lambda: defaultdict(lambda: [0, 0.0]))
    for m, e, b, z, w in cells:
        cov[m][e][0] += 1
        cov[m][e][1] += w if BENCH_GROUP[b] == "coding" else 0.0
    ncod = {m: sum(1 for c in cells if c[0] == m and BENCH_GROUP[c[2]] == "coding") for m in F.models}

    out_models = []
    for mid in sorted(set(F.models) | focus):
        mdl = models.get(mid, {})
        routes = mdl.get("orrery_routes") or []
        route_eff = mdl.get("route_efforts")
        measured_e = sorted(cov[mid].keys(), key=EFFORTS.index) if mid in cov else []
        efforts = list(route_eff) if route_eff else [e for e in measured_e if e != "unspecified"] or ["unspecified"]
        efforts = sorted(set(efforts) | set(measured_e), key=EFFORTS.index)
        pts = []
        for e in efforts:
            th = point(F, mid, e)
            bs = [point(B, mid, e) for B in boots if mid in B.theta]
            bmu = sum(bs) / len(bs) if bs else th
            bvar = sum((x - bmu) ** 2 for x in bs) / (len(bs) - 1) if len(bs) > 1 else 0.0
            se = math.sqrt(bvar + lap_var(F, mid, e))
            cidx, cse, csrc = cost_point(CM, models, mid, e)
            if e in cov[mid]:
                status = "measured"
            elif mid in cov:
                status = "extrapolated (model measured at other efforts)"
            else:
                status = "prior only (no benchmark data)"
            pts.append({
                "effort": e, "routable": bool(route_eff) and e in route_eff,
                "theta": round(th, 4), "se": round(se, 4),
                "n_cells": cov[mid][e][0] if mid in cov else 0,
                "status": status,
                "log_cost": None if cidx is None else round(cidx, 4),
                "log_cost_se": None if cse is None else round(cse, 4),
                "cost_usd_per_task": None if cidx is None else round(math.exp(cidx), 4),
                "cost_source": csrc,
            })
        out_models.append({
            "id": mid, "focus": mid in focus, "release_date": mdl.get("release_date"),
            "orrery_routes": routes, "route_efforts": route_eff,
            "theta_model": round(F.theta.get(mid, F.trend[0]), 4),
            "n_cells": F.nobs.get(mid, 0), "n_coding_cells": ncod.get(mid, 0),
            "blended_price_usd_per_mtok": blended_price(mdl),
            "points": pts,
        })

    # sanity: effort monotonicity on CursorBench / DeepSWE (raw) and in fitted theta
    mono = []
    for bench, met in (("cursorbench", "score"), ("deepswe", "pass@1")):
        raw = defaultdict(dict)
        for o in obs:
            if o["benchmark"] == bench and o["metric"] == met and o["model"] and o["effort"] in ORD:
                raw[o["model"]][o["effort"]] = o["value"]
        for m, d in sorted(raw.items()):
            es = sorted(d, key=EFFORTS.index)
            if len(es) < 2:
                continue
            raw_ok = all(d[es[i]] <= d[es[i + 1]] + 1e-9 for i in range(len(es) - 1))
            th = [point(F, m, e) for e in es]
            fit_ok = all(th[i] <= th[i + 1] + 1e-9 for i in range(len(es) - 1))
            mono.append({"benchmark": bench, "model": m, "efforts": es, "raw_monotone": raw_ok,
                         "theta_monotone": fit_ok, "theta": [round(t, 3) for t in th]})

    out = {
        "generated_from": "capability.py",
        "settings": {"tau": TAU, "sd_theta": SD_THETA, "sd_g": SD_G, "iters": ITERS, "bootstrap": BOOT,
                     "seed": SEED, "price_blend_in_out": [BLEND_IN, BLEND_OUT]},
        "benchmarks": [{"benchmark": b, "metric": met, "transform": tr, "weight": w, "group": grp,
                        "n_cells": sum(1 for c in cells if c[2] == b),
                        "z_mean": round(norm[b][0], 4), "z_sd": round(norm[b][1], 4),
                        "a": round(F.a[b], 4), "s": round(F.s[b], 4), "sigma": round(F.sig[b], 4)}
                       for (b, met), (tr, w, grp) in sorted(BENCH.items()) if b in F.a],
        "effort_offsets": {e: round(F.g[e], 4) for e in EFFORTS},
        "effort_offsets_se": {e: round(math.sqrt(sum((B.g[e] - sum(BB.g[e] for BB in boots) / len(boots)) ** 2
                                                     for B in boots) / (len(boots) - 1)), 4) for e in EFFORTS},
        "date_trend": {"theta_at_2025_06": round(F.trend[0], 4), "per_year": round(F.trend[1], 4)},
        "validation": val,
        "monotonicity": mono,
        "cost": {"benchmark_offsets": {b: round(v, 4) for b, v in CM["c_b"].items()},
                 "effort_log_multiplier_vs_high": {e: round(v, 4) for e, v in CM["h"].items()},
                 "effort_multiplier_measured_levels": CM["h_measured"],
                 "token_constant_k0": round(CM["k0"], 4), "measurement_sd": round(CM["meas_sd"], 4),
                 "estimate_sd_same_model": round(CM["tok_sd"], 4),
                 "estimate_sd_price_only": round(math.sqrt(CM["tok_sd"] ** 2 + CM["km_sd"] ** 2), 4),
                 "note": "log_cost = ln(USD per CursorBench-equivalent task); price blend 3 input : 1 output"},
        "models": out_models,
    }
    with open(OUT, "w") as f:
        json.dump(out, f, indent=1, sort_keys=False)
        f.write("\n")
    print(f"wrote {OUT}", file=sys.stderr)


if __name__ == "__main__":
    main()
