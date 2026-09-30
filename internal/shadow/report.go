package shadow

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/store"
)

// Check compares one classifier answer with the harness's own decision or the
// ground truth that arrived later. Certainty is the classifier's confidence in
// [0,1]: the reported confidence for a choice, and distance from 0.5 (scaled)
// for a noul.
type Check struct {
	Name      string  `json:"name"`
	Agree     bool    `json:"agree"`
	Certainty float64 `json:"certainty"`
}

// Checks returns the comparisons an observation supports. An unanswered
// observation, or one whose baseline or outcome has not landed, has none.
func Checks(r store.ShadowRecord) []Check {
	var answers map[string]jev.Answer
	if r.Answers == "" || json.Unmarshal([]byte(r.Answers), &answers) != nil {
		return nil
	}
	baseline, outcome := object(r.Baseline), object(r.Outcome)
	var out []Check
	switch r.Site {
	case StallJudge:
		if intervene, ok := baseline["intervene"].(bool); ok {
			if p, ok := noul(answers["stuck"]); ok {
				out = append(out, Check{"stuck_vs_llm_judge", (p >= .5) == intervene, noulCertainty(p)})
			}
		}
	case Turn:
		// Review workers are routed as review by construction, whatever they
		// are doing, so disagreeing with that phase is not a routing error.
		// Turns whose phase Jev chose from the user's message would agree
		// with Jev by construction.
		if routed, ok := baseline["routed_phase"].(string); ok && answers["phase"].Choice != "" && baseline["point"] != reviewPoint && baseline["phase_source"] != "instruction:jev" {
			out = append(out, Check{"phase_vs_routed", answers["phase"].Choice == routed, confidence(answers["phase"])})
		}
	case ReviewRisk:
		if pass, ok := outcome["pass"].(bool); ok {
			if p, ok := noul(answers["introduces_bug"]); ok {
				out = append(out, Check{"bug_vs_review", (p >= .5) == !pass, noulCertainty(p)})
			}
		}
	case ReviewVerdict:
		if pass, ok := outcome["pass"].(bool); ok {
			if c := answers["verdict"].Choice; c == "pass" || c == "fail" {
				out = append(out, Check{"verdict_vs_retry", (c == "pass") == pass, confidence(answers["verdict"])})
			}
		}
	}
	return out
}

// Difficulty returns a score answer normalised to [0,1].
func Difficulty(r store.ShadowRecord) (float64, bool) {
	var answers map[string]jev.Answer
	if r.Answers == "" || json.Unmarshal([]byte(r.Answers), &answers) != nil {
		return 0, false
	}
	a := answers["difficulty"]
	if a.Score == nil || len(a.Legend) < 2 {
		return 0, false
	}
	return *a.Score / float64(len(a.Legend)-1), true
}

// reviewPoint mirrors router.ReviewCreation without importing the router.
const reviewPoint = "review"

type bucketStats struct{ n, agree int }

type checkStats struct {
	bucketStats
	buckets [3]bucketStats
}

// certaintyBuckets are the bands thresholds get chosen from: below 0.5 is a
// coin flip, 0.8 and above is where acting automatically becomes plausible.
var certaintyBuckets = []struct {
	label string
	min   float64
}{{"<0.5", 0}, {"0.5-0.8", .5}, {">=0.8", .8}}

func bucketFor(c float64) int {
	for i := len(certaintyBuckets) - 1; i >= 0; i-- {
		if c >= certaintyBuckets[i].min {
			return i
		}
	}
	return 0
}

// WriteReport summarises observations: volume, errors and latency per site,
// agreement per check broken down by classifier certainty, the stall-kind mix,
// and how difficulty scores line up with the tier the router chose and with
// worker outcomes.
func WriteReport(w io.Writer, records []store.ShadowRecord) {
	type siteStats struct {
		n, answered, errors int
		latencies           []int64
	}
	sites := map[string]*siteStats{}
	checks := map[string]*checkStats{}
	stallKinds := map[string]map[string]int{}
	difficultyBy := map[string][]float64{}
	for _, r := range records {
		s := sites[r.Site]
		if s == nil {
			s = &siteStats{}
			sites[r.Site] = s
		}
		s.n++
		switch {
		case r.Error != "":
			s.errors++
		case r.Answers != "":
			s.answered++
		}
		if r.LatencyMS != nil && r.Error == "" {
			s.latencies = append(s.latencies, *r.LatencyMS)
		}
		for _, c := range Checks(r) {
			cs := checks[c.Name]
			if cs == nil {
				cs = &checkStats{}
				checks[c.Name] = cs
			}
			b := &cs.buckets[bucketFor(c.Certainty)]
			cs.n, b.n = cs.n+1, b.n+1
			if c.Agree {
				cs.agree, b.agree = cs.agree+1, b.agree+1
			}
		}
		if r.Site == StallJudge {
			var answers map[string]jev.Answer
			if json.Unmarshal([]byte(r.Answers), &answers) == nil && answers["stall_kind"].Choice != "" {
				verdict := "judge_unavailable"
				if v, ok := object(r.Baseline)["intervene"].(bool); ok {
					verdict = map[bool]string{true: "judge_intervened", false: "judge_declined"}[v]
				}
				if stallKinds[verdict] == nil {
					stallKinds[verdict] = map[string]int{}
				}
				stallKinds[verdict][answers["stall_kind"].Choice]++
			}
		}
		if d, ok := Difficulty(r); ok {
			var key string
			switch r.Site {
			case Turn:
				key = "turn, routed tier " + str(object(r.Baseline)["tier"])
			case Spawn:
				status := str(object(r.Outcome)["status"])
				if status == "" {
					status = "running"
				}
				key = "spawn, tier " + str(object(r.Baseline)["tier"]) + ", worker " + status
			}
			difficultyBy[key] = append(difficultyBy[key], d)
		}
	}

	fmt.Fprintf(w, "Shadow observations: %d\n\n", len(records))
	fmt.Fprintf(w, "%-16s %6s %8s %6s %8s %8s\n", "site", "n", "answered", "errors", "p50 ms", "p95 ms")
	for _, name := range sortedKeys(sites) {
		s := sites[name]
		fmt.Fprintf(w, "%-16s %6d %8d %6d %8s %8s\n", name, s.n, s.answered, s.errors, percentile(s.latencies, .5), percentile(s.latencies, .95))
	}
	if len(checks) > 0 {
		fmt.Fprintf(w, "\nAgreement (by classifier certainty)\n%-20s %10s", "check", "overall")
		for _, b := range certaintyBuckets {
			fmt.Fprintf(w, " %14s", b.label)
		}
		fmt.Fprintln(w)
		for _, name := range sortedKeys(checks) {
			cs := checks[name]
			fmt.Fprintf(w, "%-20s %10s", name, rate(cs.bucketStats))
			for _, b := range cs.buckets {
				fmt.Fprintf(w, " %14s", rate(b))
			}
			fmt.Fprintln(w)
		}
	}
	if len(stallKinds) > 0 {
		fmt.Fprintln(w, "\nStall kind by LLM judge verdict")
		for _, verdict := range sortedKeys(stallKinds) {
			kinds := stallKinds[verdict]
			parts := make([]string, 0, len(kinds))
			for _, k := range sortedKeys(kinds) {
				parts = append(parts, fmt.Sprintf("%s=%d", k, kinds[k]))
			}
			fmt.Fprintf(w, "  %-18s %s\n", verdict, strings.Join(parts, " "))
		}
	}
	if len(difficultyBy) > 0 {
		fmt.Fprintln(w, "\nMean difficulty (0 trivial .. 1 hard)")
		for _, key := range sortedKeys(difficultyBy) {
			ds := difficultyBy[key]
			sum := 0.0
			for _, d := range ds {
				sum += d
			}
			fmt.Fprintf(w, "  %-44s %.2f (n=%d)\n", key, sum/float64(len(ds)), len(ds))
		}
	}
}

func rate(b bucketStats) string {
	if b.n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(b.agree)/float64(b.n), b.agree, b.n)
}

func percentile(values []int64, p float64) string {
	if len(values) == 0 {
		return "-"
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return fmt.Sprint(sorted[min(len(sorted)-1, int(p*float64(len(sorted))))])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func object(raw store.RawJSON) map[string]any {
	var m map[string]any
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func noul(a jev.Answer) (float64, bool) {
	if a.Noul == nil {
		return 0, false
	}
	return *a.Noul, true
}

func noulCertainty(p float64) float64 {
	if p < .5 {
		return 2 * (.5 - p)
	}
	return 2 * (p - .5)
}

func confidence(a jev.Answer) float64 {
	if a.Confidence == nil {
		return 0
	}
	return *a.Confidence
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
