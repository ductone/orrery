#!/usr/bin/env python3
"""Fetch raw LLM benchmark data and rebuild the normalized modelbench files.

Stdlib only. Downloads go to raw/ (gitignored); downloaded content is only
parsed as data, never executed. Usage:

    python3 -I research/modelbench/fetch.py            # fetch + rebuild
    python3 -I research/modelbench/fetch.py --offline  # rebuild from raw/
"""

import argparse
import collections
import csv
import datetime as dt
import io
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
RAW = os.path.join(HERE, "raw")
REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
CATALOG = os.path.expanduser("~/.orrery/catalog/catalog-ramp.json")
MODELS_GO = os.path.join(REPO, "internal", "model", "models.go")

EPOCH_URL = "https://epoch.ai/data/benchmark_data.zip"
OPENROUTER_URL = "https://openrouter.ai/api/v1/models"
SWEBENCH_URL = "https://raw.githubusercontent.com/SWE-bench/swe-bench.github.io/master/data/leaderboards.json"
LIVEBENCH_URL = "https://livebench.ai/table_2025_05_30.csv"
HF_API = "https://datasets-server.huggingface.co"
LMARENA_DS = "lmarena-ai/leaderboard-dataset"
UA = "orrery-modelbench/1 (research data fetch)"

EFFORTS = ["none", "minimal", "low", "medium", "high", "xhigh", "max"]
UNSPEC = "unspecified"

# Models we always report coverage for in the summary.
FOCUS = [
    "claude-opus-5.5", "claude-sonnet-5.5", "claude-haiku-5.5", "claude-fable-5.1",
    "gpt-6.1-sol", "gpt-6-sol", "gpt-6-astra", "gpt-6-luna", "gpt-5.6-sol",
    "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5-mini", "grok-4.7", "deepseek-v4.1-flash",
    "deepseek-v4-pro", "glm-5.3", "kimi-k3", "minimax-m3", "qwen3.8-max",
]

# ---------------------------------------------------------------------------
# Effort parsing

# Word (lowercase) -> normalized level. None means "recognized effort-ish
# marker that carries no level" (-> unspecified, kept in effort_raw).
EFFORT_WORDS = {
    "none": "none", "minimal": "minimal", "low": "low",
    "medium": "medium", "med": "medium", "high": "high", "xhigh": "xhigh",
    "x-high": "xhigh", "extra-high": "xhigh", "extrahigh": "xhigh",
    "max": "max", "maximum": "max",
    "non-reasoning": "none", "nonreasoning": "none", "no-thinking": "none",
    "nothinking": "none", "non-thinking": "none", "instant": "none",
    "thinking": None, "reasoning": None, "unknown": None, "enabled": None,
    "default": None, "effort": None,
}
LEVEL_THINKING_RE = re.compile(r"^(minimal|low|medium|high|xhigh|max)thinking$")
MONTH_DATE_RE = re.compile(r"-(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*-20\d\d(?=$|-)")
BUDGET_RE = re.compile(r"^\d+(\.\d+)?k$")


def effort_word(w):
    """Normalize one effort word. Returns (is_effort, level_or_None)."""
    w = w.strip().lower().replace(" ", "-").replace("_", "-")
    if w in EFFORT_WORDS:
        return True, EFFORT_WORDS[w]
    if BUDGET_RE.match(w):
        return True, None
    m = LEVEL_THINKING_RE.match(w)
    if m:
        return True, m.group(1)
    return False, None


def effort_from_value(v):
    """Normalize a dedicated effort column value (e.g. 'Extra High', '0.99')."""
    if v is None:
        return None
    s = str(v).strip()
    if not s:
        return None
    ok, lvl = effort_word(s)
    if ok:
        return lvl or UNSPEC
    parts = re.split(r"[\s\-_]+", s.lower())
    lvls = [EFFORT_WORDS.get(p) for p in parts if p in EFFORT_WORDS]
    lvls = [x for x in lvls if x]
    if len(set(lvls)) == 1:
        return lvls[0]
    return UNSPEC


def combine_levels(levels):
    lv = [x for x in levels if x]
    if not lv:
        return UNSPEC
    if len(set(lv)) == 1:
        return lv[0]
    return UNSPEC  # conflicting markers: never guess


# ---------------------------------------------------------------------------
# Model-name normalization

CLAUDE_FAMS = "opus|sonnet|haiku|fable|mythos"
# Families whose "X-Y" (single-digit Y) means version X.Y.
DOT_FAMILIES = re.compile(
    r"^(claude-(?:%s)|gpt|grok|gemini|glm|kimi-k|minimax-m|mimo-v|muse-spark|qwen|deepseek-v|mistral-(?:large|medium|small))-?(\d+)-(\d)(?=$|-)" % CLAUDE_FAMS
)
DATE8 = re.compile(r"-(20\d\d)-?(\d\d)-?(\d\d)(?=$|-)")


def _strip_vendor(s):
    s = s.strip()
    if s.startswith("http"):
        s = s.rstrip("/").rsplit("/", 1)[-1]
    if "/" in s:
        s = s.rsplit("/", 1)[-1]
    return s


def parse_name(raw, source=None):
    """Split a raw model string into (normalized_name, effort, effort_raw, harness).

    normalized_name is the generic slug before alias lookup. effort is a level
    from EFFORTS or 'unspecified'; effort_raw is the exact fragment(s) that
    carried effort information, or None.
    """
    s = _strip_vendor(raw)
    frags, levels = [], []
    harness = None
    date_suffix = None
    extra_words = []

    # Parenthesized groups: "(High)", "(codex-harness)", "(0813)", "(thinking-minimal)"
    for g in re.findall(r"\(([^)]*)\)", s):
        gl = g.strip().lower()
        if "harness" in gl:
            harness = re.sub(r"[-\s]*harness", "", gl).strip() or gl
        elif re.fullmatch(r"\d{4}", gl):
            date_suffix = gl
        else:
            words = re.split(r"[\s\-_,]+", gl)
            words = [w for w in words if w]
            # "Extra High" -> xhigh
            joined = "-".join(words)
            ok, lvl = effort_word(joined)
            if ok:
                frags.append(g)
                levels.append(lvl)
            elif words and all(effort_word(w)[0] or w == "extra" for w in words):
                frags.append(g)
                ws = joined.replace("extra-high", "xhigh")
                levels.append(combine_levels([effort_word(w)[1] for w in ws.split("-")]))
            else:
                extra_words.append(gl.replace(" ", "-"))
    s = re.sub(r"\([^)]*\)", " ", s)

    # Epoch-style "<slug>_<effort>" suffix.
    if "_" in s:
        base, suf = s.rsplit("_", 1)
        sl = suf.strip().lower()
        m = re.fullmatch(r"pro(none|minimal|low|medium|high|xhigh|max|unknown)", sl)
        if m:
            s = base + "-pro"
            frags.append(suf)
            levels.append(EFFORT_WORDS[m.group(1)])
        else:
            ok, lvl = effort_word(sl)
            if ok:
                s = base
                frags.append(suf)
                levels.append(lvl)

    s = s.strip().lower()
    s = re.sub(r"[\s_]+", "-", s)
    s = re.sub(r"-+", "-", s).strip("-")
    for k in ("extra-high", "x-high", "non-reasoning", "no-thinking", "non-thinking"):
        if re.search(r"(^|-)%s($|-)" % k, s):
            frags.append(k)
            levels.append(EFFORT_WORDS[k])
            s = re.sub(r"(^|-)%s(?=$|-)" % k, "", s)

    # Dates: YYYYMMDD / YYYY-MM-DD. DeepSeek uses MMDD to distinguish
    # checkpoints (v4-flash-0731, v4-pro-0813), so keep MMDD there.
    def _date(m):
        if s.startswith("deepseek"):
            return "-" + m.group(2) + m.group(3)
        return ""
    s = DATE8.sub(_date, s)
    s = MONTH_DATE_RE.sub("", s)  # Epoch display names: "Gemini 2.5 Pro (Jun 2025)"
    s = re.sub(r"(?<=\d)p(?=\d)", ".", s)  # fireworks/ramp style 5p3 -> 5.3
    s = re.sub(r"^qwen-(\d)", r"qwen\1", s)

    toks = [t for t in s.split("-") if t]
    # Effort markers are only taken from the trailing run of effort/budget/
    # MMDD tokens ("...-high-32k", "...-high-0813"), so size words inside a
    # name ("phi-3-medium-4k-instruct") survive.
    j = len(toks)
    while j - 1 > 0 and (effort_word(toks[j - 1])[0] or re.fullmatch(r"\d{4}", toks[j - 1])
                         or toks[j - 1] in ("preview", "exp")):
        j -= 1
    # OpenRouter slugs use level words as model names ("fugu-max"); only the
    # o-series carry a level suffix ("o4-mini-high"). Thinking markers still strip.
    keep_levels = source == "openrouter" and not re.match(r"^o\d", toks[0] if toks else "")
    kept = []
    eff_idx = set()
    for i, t in enumerate(toks):
        ok, lvl = effort_word(t)
        is_budget = BUDGET_RE.match(t) is not None
        if i == 0 or i < j or not ok or (keep_levels and lvl is not None):
            kept.append(t)
            continue
        if is_budget:
            # only a thinking budget when next to another effort marker
            nb = (i - 1 in eff_idx) or (i + 1 < len(toks) and effort_word(toks[i + 1])[0] and not BUDGET_RE.match(toks[i + 1]))
            if not nb:
                kept.append(t)
                continue
        if t == "max" and kept and re.match(r"^qwen[\d.]*$", kept[-1]):
            kept.append(t)  # "qwen3.8-max" / "qwen-max" are model names
            continue
        if t == "medium" and re.match(r"^(mistral|magistral|devstral|phi)", "-".join(kept)):
            kept.append(t)  # "mistral-medium", "phi-3-medium"
            continue
        eff_idx.add(i)
        frags.append(t)
        levels.append(lvl)
    if source == "livebench" and kept and kept[-1] == "base":
        kept.pop()
        frags.append("base")
        levels.append("none")  # LiveBench '-base' = thinking disabled
    s = "-".join(kept)

    # Version separators: 5p5 -> 5.5, claude-opus-5-5 -> claude-opus-5.5
    s = re.sub(r"(?<=\d)p(?=\d)", ".", s)
    s = re.sub(r"^qwen-(\d)", r"qwen\1", s)
    for _ in range(2):
        s = DOT_FAMILIES.sub(lambda m: "%s-%s.%s" % (m.group(1), m.group(2), m.group(3)) if not m.group(1).endswith(("kimi-k", "minimax-m", "mimo-v", "deepseek-v")) and not m.group(1).startswith("qwen") else "%s%s.%s" % (m.group(1), m.group(2), m.group(3)), s)
    s = re.sub(r"^qwen-(\d)", r"qwen\1", s)
    # claude-3.5-sonnet / claude-4-opus -> claude-sonnet-3.5 / claude-opus-4
    s = re.sub(r"^claude-(\d+)(?:[.-](\d))?-(%s)(?=$|-)" % CLAUDE_FAMS,
               lambda m: "claude-%s-%s%s" % (m.group(3), m.group(1), "." + m.group(2) if m.group(2) else ""), s)
    if extra_words:
        s = s + "-" + "-".join(extra_words)
    if date_suffix:
        s = s + "-" + date_suffix
    s = re.sub(r"-+", "-", s).strip("-")

    effort = combine_levels(levels) if frags else UNSPEC
    effort_raw = " ".join(frags) if frags else None
    return s, effort, effort_raw, harness


# ---------------------------------------------------------------------------
# IO helpers

def http_get(url, dest, tries=4):
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    last = None
    for i in range(tries):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=120) as r:
                data = r.read()
            tmp = dest + ".tmp"
            with open(tmp, "wb") as f:
                f.write(data)
            os.replace(tmp, dest)
            return len(data)
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(2 * (i + 1) if "429" not in str(e) else 10 * (i + 1))
    raise RuntimeError("fetch failed: %s: %s" % (url, last))


def http_json(url, tries=5):
    last = None
    for i in range(tries):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=60) as r:
                return json.loads(r.read().decode("utf-8"))
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(10 * (i + 1) if "429" in str(e) else 2 * (i + 1))
    raise RuntimeError("fetch failed: %s: %s" % (url, last))


def now_iso():
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def fnum(v):
    if v is None:
        return None
    if isinstance(v, (int, float)):
        return float(v)
    s = str(v).strip().replace(",", "").replace("$", "").replace("%", "")
    if not s or s.lower() in ("nan", "none", "null", "-", "n/a"):
        return None
    try:
        x = float(s)
    except ValueError:
        return None
    if x != x:
        return None
    return x


def rnd(x, n=6):
    return None if x is None else round(x, n)


def date_only(v):
    if not v:
        return None
    s = str(v).strip()
    m = re.match(r"^(\d{4})-(\d{2})-(\d{2})", s)
    if m:
        return m.group(0)
    m = re.match(r"^(\d{4})(\d{2})(\d{2})$", s)
    if m:
        return "%s-%s-%s" % m.groups()
    return None


# ---------------------------------------------------------------------------
# Fetching

def fetch_all():
    meta_path = os.path.join(RAW, "fetch_meta.json")
    meta = load_json(meta_path, {})
    jobs = [
        ("epoch", EPOCH_URL, os.path.join(RAW, "epoch", "benchmark_data.zip")),
        ("openrouter", OPENROUTER_URL, os.path.join(RAW, "openrouter", "models.json")),
        ("swebench", SWEBENCH_URL, os.path.join(RAW, "swebench", "leaderboards.json")),
        ("livebench", LIVEBENCH_URL, os.path.join(RAW, "livebench", "table_2025_05_30.csv")),
    ]
    for name, url, dest in jobs:
        try:
            n = http_get(url, dest)
            meta[name] = {"fetched_at": now_iso(), "bytes": n}
            print("fetched %-10s %9d bytes" % (name, n))
        except Exception as e:  # noqa: BLE001
            print("WARN %s: %s (keeping previous raw if any)" % (name, e))
    try:
        fetch_lmarena()
        meta["lmarena"] = {"fetched_at": now_iso()}
    except Exception as e:  # noqa: BLE001
        print("WARN lmarena: %s (keeping previous raw if any)" % e)
    write_json(meta_path, meta)


def lmarena_configs(splits):
    cfgs = sorted({s["config"] for s in splits["splits"] if s["split"] == "latest"})
    return [c for c in cfgs if c in ("text", "webdev") or c.startswith("agent") or "cod" in c]


def fetch_lmarena():
    d = os.path.join(RAW, "lmarena")
    os.makedirs(d, exist_ok=True)
    q = urllib.parse.quote(LMARENA_DS, safe="")
    splits = http_json("%s/splits?dataset=%s" % (HF_API, q))
    write_json(os.path.join(d, "splits.json"), splits)
    for cfg in lmarena_configs(splits):
        rows, off, total = [], 0, None
        while total is None or off < total:
            url = "%s/rows?dataset=%s&config=%s&split=latest&offset=%d&length=100" % (HF_API, q, cfg, off)
            page = http_json(url)
            total = page.get("num_rows_total", 0)
            got = [r["row"] for r in page.get("rows", [])]
            if not got:
                break
            rows.extend(got)
            off += len(got)
            time.sleep(0.15)
        tmp = os.path.join(d, cfg + ".latest.jsonl.tmp")
        with open(tmp, "w") as f:
            for r in rows:
                f.write(json.dumps(r, sort_keys=True) + "\n")
        os.replace(tmp, os.path.join(d, cfg + ".latest.jsonl"))
        print("fetched lmarena/%-28s %6d rows" % (cfg, len(rows)))


def load_json(p, default=None):
    try:
        with open(p) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def write_json(p, obj):
    os.makedirs(os.path.dirname(p), exist_ok=True)
    tmp = p + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, p)


# ---------------------------------------------------------------------------
# Observations

OBS_KEYS = ["source", "dataset", "benchmark", "metric", "value", "unit", "higher_is_better",
            "model", "model_raw", "effort", "effort_raw", "harness", "cost_usd", "duration_s",
            "steps", "ci_low", "ci_high", "n", "date", "url", "extra"]


class Builder:
    def __init__(self):
        self.obs = []
        self.raw_names = collections.Counter()

    def add(self, source, dataset, benchmark, metric, value, unit, hib, model_raw,
            effort=None, effort_raw=None, harness=None, cost_usd=None, duration_s=None,
            steps=None, ci_low=None, ci_high=None, n=None, date=None, url=None,
            extra=None, parse_source=None):
        if value is None or not model_raw:
            return
        name, peff, peraw, pharness = parse_name(model_raw, parse_source or source)
        if effort is None:
            effort, effort_raw = peff, peraw
        o = {
            "source": source, "dataset": dataset, "benchmark": benchmark, "metric": metric,
            "value": rnd(value), "unit": unit, "higher_is_better": hib,
            "model": None, "_norm": name, "model_raw": model_raw, "effort": effort,
            "effort_raw": effort_raw, "harness": harness or pharness,
            "cost_usd": rnd(cost_usd), "duration_s": rnd(duration_s, 2), "steps": rnd(steps, 3),
            "ci_low": rnd(ci_low), "ci_high": rnd(ci_high), "n": n, "date": date, "url": url,
            "extra": {k: v for k, v in (extra or {}).items() if v not in (None, "")} or None,
        }
        self.obs.append(o)


# Epoch files. Benchmarks with in_eci=True in benchmark_metadata.csv are taken
# automatically (score_column + scale) unless excluded; entries here override
# or add. scale: multiply raw value to get a fraction (pct = frac*100).
EPOCH_EXCLUDE = {
    # pre-2023 NLP/knowledge benchmarks: not useful for routing coding agents
    "adversarial_nli_external.csv", "arc_ai2_external.csv", "bbh_external.csv",
    "bool_q_external.csv", "common_sense_qa_2_external.csv", "gsm8k_external.csv",
    "hella_swag_external.csv", "lambada_external.csv", "mmlu_external.csv",
    "open_book_qa_external.csv", "piqa_external.csv", "science_qa_external.csv",
    "superglue_external.csv", "trivia_qa_external.csv", "wino_grande_external.csv",
    "video_mme_external.csv", "geobench_external.csv", "lech_mazur_writing_external.csv",
    "fictionlivebench_external.csv", "vpct_external.csv", "chess_puzzles.csv",
    "furniture_assembly.csv", "mystery_game_puzzles.csv", "math_level_5.csv",
    "balrog_external.csv", "simpleqa_verified.csv", "cad_eval_external.csv",
    "deepresearchbench_external.csv",
}
EPOCH_EXTRA = {
    "epoch_capabilities_index/eci_scores.csv": dict(
        benchmark="epoch_eci", model_col="Model",
        metrics=[("eci", "eci", "index", None, True)],
        ci=("eci_ci_low", "eci_ci_high"), date_col=None),
    "metr_time_horizons_external.csv": dict(
        benchmark="metr_time_horizon",
        metrics=[("Time horizon", "p50_horizon", "minutes", None, True),
                 ("Time Horizon (80%)", "p80_horizon", "minutes", None, True),
                 ("average_score", "average_score", "pct", 1.0, True)],
        ci=("CI_low", "CI_high"), ci_metric="p50_horizon"),
    "frontierswe_external.csv": dict(
        benchmark="frontierswe",
        metrics=[("Score", "score", "pct", 1.0, True), ("Best@5", "best@5", "pct", 1.0, True)]),
    "deepswe_external.csv": dict(
        benchmark="deepswe",
        metrics=[("Pass@1", "pass@1", "pct", 1.0, True), ("Pass@4", "pass@4", "pct", 1.0, True)],
        halfwidth="95% CI half-width", halfwidth_metric="pass@1"),
    "cursorbench_external.csv": dict(benchmark="cursorbench", metrics=[("Score", "score", "pct", 1.0, True)]),
    "frontiercode_external.csv": dict(benchmark="frontiercode", metrics=[("Main score", "score", "pct", 1.0, True)]),
    "webdev_arena_external.csv": dict(
        benchmark="webdev_arena_epoch", metrics=[("Arena Score", "rating", "elo", None, True)],
        ci=("95% CI Low", "95% CI High"), n_col="Votes"),
    "live_bench_external.csv": dict(
        benchmark="livebench_epoch",
        metrics=[("Global average", "global_average", "pct", 0.01, True),
                 ("Coding average", "coding_average", "pct", 0.01, True),
                 ("Reasoning average", "reasoning_average", "pct", 0.01, True)]),
    "scicode_external.csv": dict(benchmark="scicode", metrics=[("Score", "score", "pct", 1.0, True)]),
    "critpt_external.csv": dict(benchmark="critpt", metrics=[("Accuracy", "accuracy", "pct", 1.0, True)]),
    "weirdml_v3_external.csv": dict(
        benchmark="weirdml_v3", metrics=[("Score", "score", "pct", 1.0, True)],
        ci=("Score 95% CI low", "Score 95% CI high"), ci_scale=1.0),
    "gbaeval_external.csv": dict(benchmark="gbaeval", metrics=[("Overall score", "score", "pct", 1.0, True)]),
    "algotune_external.csv": dict(benchmark="algotune", metrics=[("Score", "speedup", "ratio", None, True)]),
    "ale_bench_external.csv": dict(benchmark="ale_bench", metrics=[("Performance", "performance", "rating", None, True)]),
    "vending_bench_2_external.csv": dict(benchmark="vending_bench_2", metrics=[("Score", "net_worth", "usd", None, True)]),
    "gdp_pdf_external.csv": dict(benchmark="gdp_pdf", metrics=[("GDP.pdf score", "score", "pct", 1.0, True)]),
    "enigma_eval_external.csv": dict(benchmark="enigma_eval", metrics=[("Accuracy", "accuracy", "pct", 1.0, True)],
                                     se="Accuracy Standard Error"),
}

HARNESS_COLS = ["Harness", "Agent", "Scaffold"]
EFFORT_COLS = ["Reasoning effort", "Reasoning level", "Reasoning"]
DATE_COLS = ["Date of evaluation", "Evaluation date", "Run date", "Date", "Started at",
             "Graded at", "Last updated", "Date added"]
URL_COLS = ["Source link", "Source Link", "Log viewer", "Source link (site from table)", "Source"]
SE_COLS = ["stderr", "Accuracy SE", "Accuracy Standard Error", "Pass@1 Standard Error"]
COST_TASK_COLS = ["Cost per task", "Mean cost (USD)", "Average cost (USD)", "Cost per test (USD)", "Average costs"]
COST_OTHER_COLS = {"Cost": "cost_total_usd", "Total cost (USD)": "cost_total_usd", "Spend": "spend_usd",
                   "Estimated cost (USD)": "cost_estimated_usd", "Cost per run": "cost_per_run_usd"}
STEPS_COLS = ["Mean agent steps", "Steps per task", "Average steps"]
EXTRA_COLS = {"Mean output tokens": "mean_output_tokens", "Tokens per task": "tokens_per_task",
              "Total tokens (K)": "total_tokens_k", "Output tokens (K)": "output_tokens_k",
              "Runs": "runs", "Aggregation": "aggregation", "METR version": "metr_version",
              "Release date": "release_date", "Edit format": "edit_format",
              "Percent using correct edit format": "pct_correct_edit_format",
              "Tool setting": "tool_setting", "Step budget": "step_budget", "Provider": "provider",
              "Tools": "tools", "Total tasks": "total_tasks"}


def epoch_benchmark_id(fname):
    b = os.path.basename(fname).replace(".csv", "").replace("_external", "")
    return b


def ingest_epoch(B, zpath, fetched):
    z = zipfile.ZipFile(zpath)
    names = set(z.namelist())

    def read_csv(n):
        return list(csv.DictReader(io.TextIOWrapper(z.open(n), encoding="utf-8-sig")))

    meta = read_csv("benchmark_metadata.csv")
    cfgs = {}
    for m in meta:
        f = m.get("source_file") or ""
        if m.get("in_eci") == "True" and f and f not in EPOCH_EXCLUDE and f in names:
            sc = fnum(m.get("scale")) or 1.0
            cfgs[f] = dict(benchmark=epoch_benchmark_id(f), title=m["benchmark"],
                           metrics=[(m["score_column"], "score", "pct", sc, True)])
    for f, c in EPOCH_EXTRA.items():
        base = cfgs.get(f, {})
        base.update(c)
        cfgs[f] = base

    model_meta = {}
    if "model_metadata.csv" in names:
        for r in read_csv("model_metadata.csv"):
            if r.get("model_version"):
                model_meta[r["model_version"]] = r

    files_used = []
    for f in sorted(cfgs):
        if f not in names:
            continue
        cfg = cfgs[f]
        rows = read_csv(f)
        files_used.append(f)
        hdr = set(rows[0].keys()) if rows else set()
        model_col = cfg.get("model_col", "Model version")
        hcol = next((c for c in HARNESS_COLS if c in hdr), None)
        ecol = next((c for c in EFFORT_COLS if c in hdr), None)
        dcol = cfg.get("date_col", "__auto__")
        if dcol == "__auto__":
            dcol = next((c for c in DATE_COLS if c in hdr), None)
        secol = cfg.get("se") or next((c for c in SE_COLS if c in hdr), None)
        ccol = next((c for c in COST_TASK_COLS if c in hdr), None)
        stcol = next((c for c in STEPS_COLS if c in hdr), None)
        for r in rows:
            mraw = (r.get(model_col) or "").strip()
            if not mraw:
                continue
            url = None
            for uc in URL_COLS:
                v = (r.get(uc) or "").strip()
                if v.startswith("http"):
                    url = v
                    break
            effort = effort_raw = None
            if ecol and (r.get(ecol) or "").strip():
                effort_raw = r[ecol].strip()
                effort = effort_from_value(effort_raw)
                # The model-version suffix (e.g. "_xhigh") is usually the same
                # information; prefer the dedicated column but if it carries no
                # level and the name does, fall back to the name.
                if effort == UNSPEC:
                    _, peff, peraw, _ = parse_name(mraw, "epoch")
                    if peff != UNSPEC:
                        effort, effort_raw = peff, peraw
            dur = None
            if "Average duration (hours)" in hdr:
                h = fnum(r.get("Average duration (hours)"))
                dur = h * 3600 if h is not None else None
            for dc in ("Time per case (seconds)", "Latency (seconds)"):
                if dc in hdr and dur is None:
                    dur = fnum(r.get(dc))
            extra = {}
            for col, key in COST_OTHER_COLS.items():
                if col in hdr and col != ccol:
                    extra[key] = fnum(r.get(col))
            for col, key in EXTRA_COLS.items():
                if col in hdr and (r.get(col) or "").strip():
                    v = r[col].strip()
                    extra[key] = fnum(v) if fnum(v) is not None and key not in ("release_date", "aggregation", "metr_version", "edit_format", "tool_setting", "provider", "tools") else v
            mm = model_meta.get(mraw)
            if mm and "release_date" not in extra and mm.get("date"):
                extra["release_date"] = mm["date"]
            if "date" in hdr and model_col == "Model":
                extra["release_date"] = r.get("date")
            for col, metric, unit, scale, hib in cfg["metrics"]:
                v = fnum(r.get(col))
                if v is None:
                    continue
                conv = (lambda x: x * scale * 100) if unit == "pct" else (lambda x: x)
                value = conv(v)
                lo = hi = None
                if cfg.get("ci") and metric == cfg.get("ci_metric", cfg["metrics"][0][1]):
                    a, b = fnum(r.get(cfg["ci"][0])), fnum(r.get(cfg["ci"][1]))
                    cs = cfg.get("ci_scale")
                    if a is not None and b is not None:
                        lo, hi = (a * cs * 100, b * cs * 100) if (unit == "pct" and cs) else ((conv(a), conv(b)) if unit == "pct" else (a, b))
                elif cfg.get("halfwidth") and metric == cfg.get("halfwidth_metric"):
                    hw = fnum(r.get(cfg["halfwidth"]))
                    if hw is not None:
                        lo, hi = conv(v - hw), conv(v + hw)
                elif secol and metric == cfg["metrics"][0][1] and unit == "pct":
                    se = fnum(r.get(secol))
                    if se is not None:
                        lo, hi = conv(v - 1.96 * se), conv(v + 1.96 * se)
                        extra["stderr"] = se * scale * 100
                B.add("epoch", f, cfg["benchmark"], metric, value, unit, hib, mraw,
                      effort=effort, effort_raw=effort_raw,
                      harness=(r.get(hcol) or "").strip() or None if hcol else None,
                      cost_usd=fnum(r.get(ccol)) if ccol else None, duration_s=dur,
                      steps=fnum(r.get(stcol)) if stcol else None, ci_low=lo, ci_high=hi,
                      n=int(fnum(r.get(cfg["n_col"]))) if cfg.get("n_col") and fnum(r.get(cfg["n_col"])) is not None else None,
                      date=date_only(r.get(dcol)) if dcol else None, url=url or EPOCH_URL,
                      extra=dict(extra), parse_source="epoch")
    return files_used, model_meta


def ingest_swebench(B, path):
    d = load_json(path, {})
    counts = collections.Counter()
    for board in d.get("leaderboards", []):
        bname = board.get("name", "?")
        bench = "swebench_" + re.sub(r"[^a-z0-9]+", "_", bname.lower()).strip("_")
        for r in board.get("results", []):
            mtags = [t.split(":", 1)[1].strip() for t in r.get("tags") or [] if t.startswith("Model:")]
            if len(mtags) == 1:
                mraw = mtags[0]
            elif len(mtags) > 1:
                mraw = "multiple: " + ", ".join(mtags)
            else:
                mraw = r.get("model_display") or r.get("name")
            eraw = r.get("reasoning_effort")
            multi = mraw.startswith("multiple:")
            effort = effort_from_value(eraw) if eraw else (UNSPEC if multi else None)
            extra = {"cost_total_usd": r.get("cost"), "system_name": r.get("name"),
                     "model_display": r.get("model_display"), "checked": r.get("checked"),
                     "os_model": r.get("os_model"), "folder": r.get("folder"),
                     "mini_swe_agent_version": r.get("mini-swe-agent_version"),
                     "model_release_date": date_only(r.get("model_release_date")),
                     "attempts": next((t.split("-", 1)[1].strip() for t in r.get("tags") or [] if t.startswith("System: Attempts")), None)}
            o_before = len(B.obs)
            B.add("swebench", bname, bench, "resolved", fnum(r.get("resolved")), "pct", True, mraw,
                  effort=effort, effort_raw=eraw if eraw else None, harness=r.get("agent"),
                  cost_usd=fnum(r.get("instance_cost")), steps=fnum(r.get("instance_calls")),
                  date=date_only(r.get("date")), url=r.get("site") or "https://www.swebench.com/",
                  extra=extra)
            if multi and len(B.obs) > o_before:
                B.obs[-1]["_norm"] = "multiple"
            counts[bname] += 1
    return counts


def ingest_lmarena(B, d):
    splits = load_json(os.path.join(d, "splits.json"), {"splits": []})
    cfgs = lmarena_configs(splits) if splits.get("splits") else []
    counts = {}
    for cfg in cfgs:
        p = os.path.join(d, cfg + ".latest.jsonl")
        if not os.path.exists(p):
            continue
        n = 0
        with open(p) as f:
            for line in f:
                r = json.loads(line)
                cat = r.get("category") or "overall"
                bench = "lmarena_%s:%s" % (cfg, cat)
                if "rating" in r:
                    B.add("lmarena", cfg + "/latest", bench, "rating", fnum(r.get("rating")), "elo", True,
                          r.get("model_name"), ci_low=fnum(r.get("rating_lower")), ci_high=fnum(r.get("rating_upper")),
                          n=int(r["vote_count"]) if r.get("vote_count") is not None else None,
                          date=date_only(r.get("leaderboard_publish_date")),
                          url="https://huggingface.co/datasets/" + LMARENA_DS,
                          extra={"rank": r.get("rank"), "organization": r.get("organization"), "variance": r.get("variance")})
                else:
                    B.add("lmarena", cfg + "/latest", bench, "score", fnum(r.get("score")), "score", True,
                          r.get("model_name"), ci_low=fnum(r.get("score_ci_lower")), ci_high=fnum(r.get("score_ci_upper")),
                          n=int(r["session_count"]) if r.get("session_count") is not None else None,
                          date=date_only(r.get("leaderboard_publish_date")),
                          url="https://huggingface.co/datasets/" + LMARENA_DS,
                          extra={"rank": r.get("rank"), "organization": r.get("organization"),
                                 "observation_count": r.get("observation_count")})
                n += 1
        counts[cfg] = n
    return counts


def ingest_livebench(B, path):
    if not os.path.exists(path):
        return 0
    with open(path, encoding="utf-8-sig") as f:
        rows = list(csv.DictReader(f))
    for r in rows:
        mraw = r.get("model")
        for k, v in r.items():
            if k == "model":
                continue
            B.add("livebench", os.path.basename(path), "livebench:" + k, "score", fnum(v), "pct", True, mraw,
                  date="2025-05-30", url=LIVEBENCH_URL, parse_source="livebench")
    return len(rows)


def ingest_openrouter(B, path):
    d = load_json(path, {"data": []})
    entries = {}
    for m in d.get("data", []):
        oid = m.get("id", "")
        if oid.startswith(("~", "openrouter/")) or ":" in oid:
            continue  # floating aliases (~...-latest), OpenRouter routers, :batch/:free variants
        p = m.get("pricing") or {}

        def per_m(k, src=p):
            x = fnum(src.get(k))
            return None if x is None or x < 0 else round(x * 1e6, 6)
        pricing = {"input": per_m("prompt"), "output": per_m("completion"),
                   "cache_read": per_m("input_cache_read"), "cache_write": per_m("input_cache_write")}
        if p.get("input_cache_write_1h"):
            pricing["cache_write_1h"] = per_m("input_cache_write_1h")
        tiers = []
        for ov in p.get("overrides") or []:
            if isinstance(ov, dict):
                # conditions: prompt-size thresholds and/or UTC time-of-day windows
                t = {k: ov[k] for k in ("min_prompt_tokens", "utc_days", "utc_start", "utc_end") if ov.get(k) is not None}
                for k, kk in (("prompt", "input"), ("completion", "output"), ("input_cache_read", "cache_read"), ("input_cache_write", "cache_write")):
                    if ov.get(k) is not None:
                        t[kk] = per_m(k, ov)
                tiers.append(t)
        if tiers:
            pricing["tiers"] = tiers
        sp = m.get("supported_parameters") or []
        rs = m.get("reasoning") or {}
        created = m.get("created")
        cdate = dt.datetime.fromtimestamp(created, dt.timezone.utc).strftime("%Y-%m-%d") if created else None
        e = {"openrouter_id": oid, "pricing": pricing, "context_window": m.get("context_length"),
             "reasoning_supported": ("reasoning" in sp) or bool(rs),
             "reasoning_efforts": rs.get("supported_efforts"), "default_effort": rs.get("default_effort"),
             "created": cdate, "supported_parameters": sp}
        entries[oid] = e
        for k in ("input", "output", "cache_read", "cache_write"):
            if pricing.get(k) is not None:
                B.add("openrouter", "api/v1/models", "openrouter_pricing", k + "_usd_per_mtok", pricing[k],
                      "usd_per_mtok", False, oid, date=cdate, url="https://openrouter.ai/" + oid)
        if m.get("context_length"):
            B.add("openrouter", "api/v1/models", "openrouter_context", "context_length", float(m["context_length"]),
                  "tokens", True, oid, date=cdate, url="https://openrouter.ai/" + oid)
    return entries


# ---------------------------------------------------------------------------
# Orrery routes

def orrery_routes():
    routes = []
    cat = load_json(CATALOG, {})
    for m in cat.get("models", []):
        if m.get("ID"):
            routes.append(m["ID"])
    try:
        with open(MODELS_GO) as f:
            src = f.read()
        for m in re.finditer(r'\{Provider:\s*"([^"]+)",\s*Model:\s*"([^"]+)"(?:,\s*WireID:\s*"([^"]+)")?', src):
            routes.append("%s/%s" % (m.group(1), m.group(3) or m.group(2)))
    except OSError:
        pass
    return sorted(set(routes))


def route_model(route):
    wire = route.split("/", 1)[1] if "/" in route else route
    name, _, _, _ = parse_name(wire)
    return name


# ---------------------------------------------------------------------------
# Registry

VENDORS = [
    (r"^claude", "anthropic"), (r"^(gpt|o\d|chatgpt|codex)", "openai"), (r"^grok", "xai"),
    (r"^(gemini|gemma)", "google"), (r"^deepseek", "deepseek"), (r"^(glm|chatglm)", "zhipu"),
    (r"^kimi", "moonshot"), (r"^minimax", "minimax"), (r"^(qwen|qwq|codeqwen)", "alibaba"),
    (r"^mimo", "xiaomi"), (r"^(llama|muse)", "meta"), (r"^nemotron", "nvidia"),
    (r"^(mistral|devstral|magistral|codestral|ministral)", "mistral"), (r"^step", "stepfun"),
    (r"^(seed|doubao)", "bytedance"), (r"^hy\d", "tencent"),
]


def vendor_of(mid):
    for rx, v in VENDORS:
        if re.match(rx, mid):
            return v
    return None


def family_of(mid):
    m = re.match(r"^claude-(%s)" % CLAUDE_FAMS, mid)
    if m:
        return "claude-" + m.group(1)
    m = re.match(r"^gpt-[\d.]+-(sol|terra|luna|astra)", mid)
    if m:
        return "gpt-" + m.group(1)
    m = re.match(r"^([a-z]+(?:-[a-z]+)?)", re.sub(r"[\d.]+.*$", "", mid) or mid)
    return (m.group(1).rstrip("-") if m else mid) or mid


def build(offline):
    aliases_cfg = load_json(os.path.join(HERE, "aliases.json"), {})
    aliases = aliases_cfg.get("aliases", {})
    fetch_meta = load_json(os.path.join(RAW, "fetch_meta.json"), {})
    B = Builder()
    sources = {}

    zpath = os.path.join(RAW, "epoch", "benchmark_data.zip")
    files_used, model_meta = ([], {})
    if os.path.exists(zpath):
        files_used, model_meta = ingest_epoch(B, zpath, fetch_meta.get("epoch"))
    sources["epoch"] = {"url": EPOCH_URL, "files": files_used}

    or_entries = ingest_openrouter(B, os.path.join(RAW, "openrouter", "models.json"))
    sources["openrouter"] = {"url": OPENROUTER_URL, "models_listed": len(or_entries),
                             "notes": "':batch'/':free' variants and '~' floating aliases skipped. Prices converted to USD per 1M tokens; tiered pricing from pricing.overrides."}
    swe_counts = ingest_swebench(B, os.path.join(RAW, "swebench", "leaderboards.json"))
    sources["swebench"] = {"url": SWEBENCH_URL, "boards": dict(swe_counts),
                           "notes": "Model from 'Model:' tags; multi-model systems kept with model=null. per_instance_details not ingested."}
    lma_counts = ingest_lmarena(B, os.path.join(RAW, "lmarena"))
    sources["lmarena"] = {"url": "%s/rows?dataset=%s" % (HF_API, LMARENA_DS), "configs": lma_counts,
                          "notes": "split=latest. text/webdev: Bradley-Terry rating (elo-like) with CI and vote_count; agent*: per-config score (not elo), n=session_count. Higher is better assumed for agent configs because rank 1 has the highest score."}
    lb_rows = ingest_livebench(B, os.path.join(RAW, "livebench", "table_2025_05_30.csv"))
    sources["livebench"] = {"url": LIVEBENCH_URL, "rows": lb_rows,
                            "notes": "Stale (2025-05-30 snapshot); '-base' suffix parsed as effort none, '-thinking[-Nk]' as unspecified."}

    # Registry seed: routes, OpenRouter, Epoch model metadata, extra_models.
    routes = orrery_routes()
    reg = {}

    def canon(norm):
        return aliases.get(norm, norm)

    def ensure(mid):
        if mid not in reg:
            reg[mid] = {"id": mid, "vendor": vendor_of(mid), "family": family_of(mid), "aliases": {},
                        "openrouter_id": None, "orrery_routes": [], "release_date": None, "pricing": None,
                        "pricing_source": None, "route_pricing": {}, "route_efforts": None,
                        "context_window": None, "reasoning_supported": None, "reasoning_efforts": None,
                        "sources_present": []}
        return reg[mid]

    for r in routes:
        ensure(canon(route_model(r)))["orrery_routes"].append(r)
    for oid, e in sorted(or_entries.items()):
        name, eff, _, _ = parse_name(oid, "openrouter")
        if eff != UNSPEC and eff != "none":
            continue  # e.g. openai/o4-mini-high: effort variant listing
        ent = ensure(canon(name))
        # prefer an exact slug match over a date-stripped duplicate
        exact = _strip_vendor(oid).lower() == ent["id"]
        if ent["openrouter_id"] is None or exact:
            ent["openrouter_id"] = oid
            ent["pricing"] = e["pricing"]
            ent["pricing_source"] = "openrouter"
            ent["context_window"] = e["context_window"]
            ent["reasoning_supported"] = e["reasoning_supported"]
            ent["reasoning_efforts"] = e["reasoning_efforts"]
            ent["release_date"] = ent["release_date"] or e["created"]
    for mv, mm in model_meta.items():
        name, _, _, _ = parse_name(mv, "epoch")
        if not name:
            continue
        ent = ensure(canon(name))
        if mm.get("date") and (not ent.get("_epoch_date") or mm["date"] < ent["_epoch_date"]):
            ent["_epoch_date"] = mm["date"]
        if mm.get("organization") and not ent.get("_org"):
            ent["_org"] = mm["organization"]
    for mid in aliases_cfg.get("extra_models", []):
        ensure(mid)
    # Catalog facts for routes (context window, effort levels) when OpenRouter lacks them.
    cat = load_json(CATALOG, {})
    for m in cat.get("models", []):
        ent = reg.get(canon(route_model(m["ID"])))
        if ent is None:
            continue
        if not ent["context_window"]:
            ent["context_window"] = m.get("ContextWindow")
        pr = m.get("Pricing") or {}
        rp = {"input": pr.get("Input"), "output": pr.get("Output"), "cache_read": pr.get("CacheRead"),
              "cache_write": pr.get("CacheWrite")}
        if pr.get("Thresholds"):
            rp["tiers"] = pr["Thresholds"]
        ent["route_pricing"][m["ID"]] = rp
        if ent["pricing"] is None:
            ent["pricing"], ent["pricing_source"] = dict(rp), "orrery-catalog:" + m["ID"]
        if m.get("Effort") and not ent.get("route_efforts"):
            ent["route_efforts"] = m["Effort"]
        if ent["reasoning_supported"] is None and m.get("Effort"):
            ent["reasoning_supported"] = any(x != "none" for x in m["Effort"])

    # Map observations.
    unmapped = collections.Counter()
    ok_unmapped = set(aliases_cfg.get("unmapped_ok", []))
    for o in B.obs:
        mid = canon(o.pop("_norm"))
        if mid in reg and mid not in ok_unmapped:
            o["model"] = mid
            ent = reg[mid]
            al = ent["aliases"].setdefault(o["source"], [])
            if o["model_raw"] not in al:
                al.append(o["model_raw"])
            if o["source"] not in ent["sources_present"]:
                ent["sources_present"].append(o["source"])
        else:
            unmapped[(o["source"], o["model_raw"])] += 1

    for ent in reg.values():
        ed = ent.pop("_epoch_date", None)
        ent.pop("_org", None)
        if ed:
            ent["release_date"] = ed  # Epoch's curated release date wins over OpenRouter listing date
        ent["orrery_routes"] = sorted(set(ent["orrery_routes"]))
        ent["sources_present"] = sorted(ent["sources_present"])
        for k in ent["aliases"]:
            ent["aliases"][k] = sorted(ent["aliases"][k])
    # Keep the registry focused: drop entries that nothing observed or routes to,
    # except OpenRouter-listed models and explicit extras.
    extras = set(aliases_cfg.get("extra_models", []))
    reg = {k: v for k, v in reg.items()
           if v["sources_present"] or v["orrery_routes"] or v["openrouter_id"] or k in extras}

    obs = sorted(B.obs, key=lambda o: (o["source"], o["benchmark"], o["model"] or "~", o["model_raw"],
                                       EFFORTS.index(o["effort"]) if o["effort"] in EFFORTS else 99,
                                       o["metric"], o["harness"] or "", o["date"] or "", o["value"]))
    with open(os.path.join(HERE, "observations.jsonl.tmp"), "w") as f:
        for o in obs:
            f.write(json.dumps({k: o.get(k) for k in OBS_KEYS}, sort_keys=False) + "\n")
    os.replace(os.path.join(HERE, "observations.jsonl.tmp"), os.path.join(HERE, "observations.jsonl"))
    write_json(os.path.join(HERE, "models.json"), {"generated_from": "fetch.py", "models": [reg[k] for k in sorted(reg)]})

    # Provenance.
    by_src = collections.defaultdict(list)
    for o in obs:
        by_src[o["source"]].append(o)
    for s, info in sources.items():
        lst = by_src.get(s, [])
        info["fetched_at"] = (fetch_meta.get(s) or {}).get("fetched_at")
        info["observations"] = len(lst)
        info["observations_mapped"] = sum(1 for o in lst if o["model"])
        dates = [o["date"] for o in lst if o["date"]]
        info["newest_date_in_data"] = max(dates) if dates else None
        info["benchmarks"] = dict(sorted(collections.Counter(o["benchmark"] for o in lst).items())) if s != "lmarena" else dict(sorted(collections.Counter(o["benchmark"].split(":")[0] for o in lst).items()))
    write_json(os.path.join(HERE, "sources.json"), sources)

    summarize(obs, reg, unmapped, sources, routes, canon)


def summarize(obs, reg, unmapped, sources, routes, canon):
    print("\n== observations per source")
    for s, info in sorted(sources.items()):
        print("  %-11s %7d obs (%d mapped)  newest %s  fetched %s" % (
            s, info["observations"], info["observations_mapped"], info["newest_date_in_data"], info["fetched_at"]))
    print("  models.json: %d models" % len(reg))
    print("\n== effort distribution")
    ec = collections.Counter(o["effort"] for o in obs)
    print("  " + ", ".join("%s=%d" % (e, ec[e]) for e in EFFORTS + [UNSPEC] if ec[e]))
    print("\n== orrery routes without benchmark data (excluding openrouter)")
    missing = []
    for r in routes:
        mid = canon(route_model(r))
        sp = [x for x in reg.get(mid, {}).get("sources_present", []) if x != "openrouter"]
        if not sp:
            missing.append("%s->%s" % (r, mid))
    print("  " + (", ".join(missing) or "(none)"))
    print("\n== focus coverage (benchmark: efforts)")
    cov = collections.defaultdict(lambda: collections.defaultdict(set))
    for o in obs:
        if o["model"] in FOCUS and o["source"] != "openrouter":
            b = o["benchmark"]
            if b.startswith("lmarena_text:") and not b.endswith(":overall"):
                continue
            cov[o["model"]][b].add(o["effort"])
    order = {e: i for i, e in enumerate(EFFORTS + [UNSPEC])}
    for m in FOCUS:
        parts = ["%s[%s]" % (b, ",".join(sorted(es, key=order.get))) for b, es in sorted(cov[m].items())]
        print("  %-20s %s" % (m, "; ".join(parts) if parts else "(no benchmark data)"))
    print("\n== top unmapped model_raw (source, raw: count) -- extend aliases.json")
    for (s, r), c in unmapped.most_common(40):
        print("  %-10s %-55s %d" % (s, r[:55], c))
    print("  ... %d distinct unmapped raw names" % len(unmapped))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--offline", action="store_true", help="rebuild from raw/ without fetching")
    a = ap.parse_args()
    if not a.offline:
        fetch_all()
    elif not os.path.isdir(RAW):
        sys.exit("raw/ missing; run without --offline first")
    build(a.offline)


if __name__ == "__main__":
    main()
