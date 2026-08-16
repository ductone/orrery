# Engineering benchmark

This replay set measures whether Orrery can diagnose, edit, verify, and finish small software-engineering tasks. Every case runs in a temporary copy of a synthetic fixture, so repeated runs are isolated and do not mutate the checked-in source.

Run it with a configured model provider:

```sh
orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy=v1 --output=.orrery/benchmarks/latest.json
```

Compare a candidate against a saved baseline:

```sh
orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy=candidate \
  --baseline=.orrery/benchmarks/baseline.json \
  --output=.orrery/benchmarks/candidate.json
```

The report tracks pass rate, cost per successful case, token use, median and p95 latency, tool-error rate, edit retries, verification, and independent review. A comparison exits with code 4 when pass rate falls below 97% of the baseline. Benchmark reports live under `.orrery/` and are intentionally not committed: they may contain model output or local paths.

Fixtures in this directory are synthetic and public-safe. Do not add proprietary source, private issue text, customer data, credentials, internal URLs, or captured production transcripts.
