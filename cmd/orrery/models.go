package main

// models reads the startup catalog without resolving secrets or starting the engine.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ductone/orrey/internal/catalog"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
)

// modelRow is one catalog entry as `orrery models` prints it.
type modelRow struct {
	id     string
	model  string // canonical model, empty for a discovery-only route
	tier   model.Tier
	family model.Family
	input  string // per million tokens
	output string // per million tokens
	ctx    string // context window
	source string // builtin, discovered, or overridden
}

// sourceFor preserves both discovery and override flags.
func sourceFor(m model.ModelSpec, overridden bool) string {
	if overridden && m.Discovered {
		return "discovered,overridden"
	}
	if overridden {
		return "overridden"
	}
	if m.Discovered {
		return "discovered"
	}
	return "builtin"
}

// usdPerM formats a per-million-token price.
func usdPerM(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", v)
}

// shortCtx abbreviates a context window in tokens.
func shortCtx(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%dK", n/1000)
	}
}

// listModels builds the startup catalog and prints it.
func listModels(ctx context.Context, rt *runtime, args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	statsFlag := fs.Bool("stats", false, "add per-model call statistics from the store")
	if fs.Parse(args) != nil {
		return 2
	}
	res := catalog.Build(ctx, rt.cfg, catalog.Dir(), nil)
	for _, d := range res.Discoveries {
		if d.Error != "" {
			fmt.Fprintf(os.Stderr, "models: discovery %s: %s (using the on-disk cache where present)\n", d.Provider, d.Error)
		}
	}
	var stats map[string]store.ModelStat
	var outcomes map[string]store.ModelOutcome
	if *statsFlag {
		st, err := rt.store.ModelStats(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		stats = indexStats(st)
		oc, err := rt.store.ModelOutcomes(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		outcomes = map[string]store.ModelOutcome{}
		for _, o := range oc {
			outcomes[o.Route] = o
		}
	}
	printModelRows(os.Stdout, res, rt.cfg.Models, stats, outcomes)
	return 0
}

// printModelRows writes the table, then the disabled entries and warnings.
func printModelRows(w io.Writer, res catalog.Result, overrides []config.ModelConfig, stats map[string]store.ModelStat, outcomes map[string]store.ModelOutcome) {
	overridden := overriddenIDs(overrides, res.Models)
	rows := make([]modelRow, 0, len(res.Models))
	for _, m := range res.Models {
		rows = append(rows, modelRow{
			id: m.ID, model: m.Model, tier: m.Tier, family: m.Family,
			input: usdPerM(m.Pricing.Input), output: usdPerM(m.Pricing.Output),
			ctx: shortCtx(m.ContextWindow), source: sourceFor(m, overridden[m.ID]),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if stats != nil {
		fmt.Fprintln(tw, "ID\tMODEL\tTIER\tFAMILY\tIN/1M\tOUT/1M\tCTX\tSOURCE\tCALLS\tAVG\tTOK/S\tTRUNC%\tFAILS\tSLOW\tAUTH-P/F\tREVIEWS\tOVERTURNED\tANS-REJ\tRUNS-P/F/I/C")
	} else {
		fmt.Fprintln(tw, "ID\tMODEL\tTIER\tFAMILY\tIN/1M\tOUT/1M\tCTX\tSOURCE")
	}
	for _, r := range rows {
		if stats != nil {
			st := stats[r.id]
			oc := outcomes[r.id]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\t%s\t%s\t%d\t%d\t%d\t%s\n",
				r.id, dash(r.model), r.tier, r.family, r.input, r.output, r.ctx, r.source,
				st.Calls, seconds(st.LatencySeconds), tokps(st.OutputTokensPerSecond),
				pct(st.Truncated, st.Calls), st.ProviderErrors+st.Empty+st.Malformed,
				when(st.LastSlowCall), authored(oc), oc.Reviews, oc.Overturned, oc.AnswerRejected,
				fmt.Sprintf("%d/%d/%d/%d", oc.RunPass, oc.RunFail, oc.RunInputRequired, oc.RunCancelled))
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.id, dash(r.model), r.tier, r.family, r.input, r.output, r.ctx, r.source)
		}
	}
	tw.Flush()
	if disabled := disabledIDs(overrides, res); len(disabled) > 0 {
		fmt.Fprintf(w, "\n%d disabled by config:\n", len(disabled))
		for _, id := range disabled {
			fmt.Fprintf(w, "  %s\n", id)
		}
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
}

// authored renders the weighted review pass and fail of a route's edits.
func authored(o store.ModelOutcome) string {
	return fmt.Sprintf("%.1f/%.1f", o.AuthoredPass, o.AuthoredFail)
}

// dash replaces an empty canonical model with a dash.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// seconds formats an average latency in seconds.
func seconds(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fs", v)
}

// tokps formats output tokens per second.
func tokps(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

// pct renders n of total as a percentage, or a dash when total is zero.
func pct(n, total int) string {
	if total == 0 {
		return "-"
	}
	return strconv.Itoa((n*100 + total/2) / total)
}

// when renders the coarse age of a timestamp, or a dash when unset.
func when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// overriddenIDs marks the routes the config's override entries match: by
// route id, or by canonical model when the entry has no provider prefix.
// An entry that adds a model matches the route it added by id, so a
// vouched-for model reads as overridden rather than builtin.
func overriddenIDs(overrides []config.ModelConfig, models []model.ModelSpec) map[string]bool {
	out := map[string]bool{}
	for _, o := range overrides {
		if o.Disabled != nil && *o.Disabled {
			continue
		}
		for _, m := range models {
			if m.ID == o.ID || (!strings.Contains(o.ID, "/") && m.Model == o.ID) {
				out[m.ID] = true
			}
		}
	}
	return out
}

// disabledIDs lists the route ids the config's disabled entries removed, in
// config order and deduplicated as catalog.Apply counts them. A disabled
// route is no longer in the built catalog, so the ids are matched against
// the pre-override catalog, rebuilt exactly as catalog.Build had it:
// discovery minus refused models, then the built-in routes.
func disabledIDs(overrides []config.ModelConfig, res catalog.Result) []string {
	base := preOverrideCatalog(res)
	var out []string
	seen := map[string]bool{}
	for _, o := range overrides {
		if o.Disabled == nil || !*o.Disabled {
			continue
		}
		for _, m := range base {
			if (m.ID == o.ID || (!strings.Contains(o.ID, "/") && m.Model == o.ID)) && !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m.ID)
			}
		}
	}
	return out
}

// preOverrideCatalog rebuilds the catalog as it stood after Merge, before
// Apply removed the disabled routes.
func preOverrideCatalog(res catalog.Result) []model.ModelSpec {
	discovered := discoveredSpecs(res)
	refused := catalog.Unavailable(catalog.Dir(), time.Now())
	discovered = slices.DeleteFunc(discovered, func(m model.ModelSpec) bool { _, ok := refused[m.ID]; return ok })
	base, _ := catalog.Merge(discovered, model.Catalog)
	return base
}

// discoveredSpecs flattens the discovery records into one spec list,
// matching what catalog.Build collected before Merge reshaped it.
func discoveredSpecs(res catalog.Result) []model.ModelSpec {
	var out []model.ModelSpec
	for _, d := range res.Discoveries {
		out = append(out, d.Models...)
	}
	return out
}

// indexStats keys model statistics by route id.
func indexStats(st []store.ModelStat) map[string]store.ModelStat {
	out := make(map[string]store.ModelStat, len(st))
	for _, s := range st {
		out[s.Route] = s
	}
	return out
}
