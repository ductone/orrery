# modelbench

A local, JSON-only database of raw public LLM benchmark data, keyed by the
same `(model, effort)` pair that orrery routes on (`route@effort`). It is input
for routing research; nothing in orrery reads it at runtime.

## Refresh

    python3 -I research/modelbench/fetch.py            # download into raw/ (gitignored), rebuild
    python3 -I research/modelbench/fetch.py --offline  # rebuild from raw/ only

Stdlib only. Each run is idempotent: the same `raw/` always produces the same
output files. A source that fails to download keeps its previous `raw/` copy.
The run prints a summary: counts per source, effort distribution, orrery routes
without benchmark data, coverage for the focus models, and the most common
unmapped `model_raw` names. Add those to `aliases.json` to map them.

## Files

- `fetch.py`: fetchers, the name/effort normalizer, and the builders.
- `aliases.json`: hand-maintained table. `aliases` maps a normalized name to a canonical id. `extra_models` must always exist. `unmapped_ok` names are deliberately left unmapped.
- `models.json` (generated): the canonical registry.
- `observations.jsonl` (generated): one measurement per line.
- `sources.json` (generated): provenance for each source (URL, fetched_at, counts, newest date in the data, notes).

| source | what |
|---|---|
| epoch | `benchmark_data.zip`. ECI, plus every in-ECI benchmark in `benchmark_metadata.csv` except pre-2023 NLP sets, and extras such as CursorBench, FrontierCode, SciCode, CritPt and WeirdML v3 (see `EPOCH_EXTRA` / `EPOCH_EXCLUDE`) |
| openrouter | `/api/v1/models`. Prices and context as observations, and also on the registry |
| lmarena | HF `lmarena-ai/leaderboard-dataset`, split `latest`. Configs `text` (all categories), `webdev`, `agent*` |
| swebench | `leaderboards.json`. Every board |
| livebench | `table_2025_05_30.csv` (stale, included because it was trivial) |

## observations.jsonl

`source, dataset, benchmark, metric, value, unit, higher_is_better, model,
model_raw, effort, effort_raw, harness, cost_usd, duration_s, steps, ci_low,
ci_high, n, date, url, extra`

- `model` is a canonical id, or `null` when it is unmapped. `model_raw` is always the exact string from the source.
- `unit`:
  - `pct` is 0–100. Epoch fractions are multiplied out using `benchmark_metadata.scale`.
  - `elo` is the LMArena or Arena rating.
  - `score` is an LMArena agent score, which is not elo.
  - `index` is ECI.
  - `minutes` is the METR horizon.
  - `usd_per_mtok` and `tokens` come from OpenRouter.
- `cost_usd` is per task or instance, and only when the source says so. Run totals go in `extra.cost_total_usd`.
- `duration_s` is in seconds per task. `steps` is agent steps (or API calls) per task.
- `ci_low` and `ci_high` are in the same unit as `value`. When a source gives only a standard error, they are value ± 1.96·SE, and the SE is kept in `extra.stderr`.
- `n` is the vote count for LMArena ratings, or the session count for agent configs.
- `date` is the evaluation or publish date when the source gives one. A model's release date is in `extra.release_date` or `models.json`.
- `harness` is the agent or scaffold when the source names one, e.g. `mini-swe-agent`, `claude-code`, `codex`, `proximus`, or the LMArena `(codex-harness)` tag.

## models.json

Each entry holds:

- `id`, `vendor`, `family`
- `aliases` {source: [raw names]}
- `openrouter_id`
- `orrery_routes`: from `~/.orrery/catalog/catalog-ramp.json` and the `internal/model/models.go` routes
- `release_date`: Epoch's date, otherwise the OpenRouter `created` date
- `pricing` in USD per 1M tokens, with `tiers` for prompt-size or UTC time-window overrides
- `pricing_source`: openrouter, otherwise orrery-catalog
- `route_pricing` and `route_efforts`: from the orrery catalog
- `context_window`, `reasoning_supported`, `reasoning_efforts` (OpenRouter)
- `sources_present`

The registry is seeded from orrery routes, OpenRouter, Epoch `model_metadata.csv` and `extra_models`.

## Effort normalization

The scale runs `none < minimal < low < medium < high < xhigh < max`. `unspecified` means the source gave no level; it is a real value, never a guess. Rows with different efforts are never merged.

Where effort comes from:

- **Dedicated columns** win: Epoch `Reasoning effort` / `Reasoning level` / `Reasoning`, and SWE-bench `reasoning_effort`.
- **Otherwise the name is parsed** and the effort is stripped from the canonical id. `effort_raw` keeps the exact fragments. The name forms are:
  - an Epoch `_suffix`, e.g. `claude-opus-5-5_max`
  - `_promax`, which means model `-pro` at effort `max`
  - parenthesized groups, e.g. `GPT 6.1 Sol (Max)` or `(thinking-minimal)`
  - a trailing hyphen run, e.g. `-xhigh`, `-high-32k`, `-high-0813` or `-thinking-high-effort`

Words and what they map to:

- `Extra High` / `x-high` → `xhigh`.
- `non-reasoning`, `no-thinking`, `nothinking` and `instant` → `none`. LiveBench `-base` → `none`.
- `thinking`, `reasoning`, `enabled`, `unknown`, `0.99` and bare token budgets (`32K`, `24K thinking`) → `unspecified`. No source documents how a budget maps to a level.
- `<level>thinking` (LiveBench `highthinking`) → that level.

Effort words are stripped only from the trailing run of the name, so model names survive: `qwen3.8-max`, `qwen-max`, `mistral-medium`, `phi-3-medium-4k-instruct`. OpenRouter slugs keep level words (`fugu-max`); only the o-series `-high` suffix is parsed.

Ids are normalized as follows:

- Lowercase, with the vendor prefix dropped.
- `5p3` / `5-5` → `5.3` / `5.5`.
- `claude-3-5-sonnet` → `claude-sonnet-3.5`.
- 8-digit and ISO dates are dropped, except for DeepSeek, which keeps `MMDD` (`deepseek-v4-pro-0813`). 4-digit `MMDD` suffixes are kept.

## Known gaps / caveats

- **SWE-bench** (newest 2026-02-26) and **LiveBench** (2025-05) are stale. Epoch's Terminal-Bench rows stop at GPT-5.5, so none of the 2026-H2 focus models have Terminal-Bench or SWE-bench-site data.
- **LMArena** usually lists one effort per model, typically max or high. Its `agent*` scores are not elo, and their direction is inferred from rank order.
- **ECI** is per model, so its effort is `unspecified`.
- **No route has two prices.** Thinking and non-thinking variants (grok-4.20 reasoning vs non-reasoning) share one registry id, and their routes are both listed. No source has data for `glm-5.2-fast` or `kimi-k3-fast`.
- **Previews stay unmapped.** Preview checkpoints (`deepseek-v4-pro-preview`, `qwen3.7-max-preview`) and multi-model SWE-bench systems (`model=null`, `model_raw="multiple: ..."`) are left unmapped on purpose.
- **Artificial Analysis** is not included because it needs an API key.
