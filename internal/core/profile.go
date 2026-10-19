package core

// Profile is the domain part of the system prompt: who the agent is and the
// guidance for the kind of work it does. The engine owns the rest (its
// harness rules: tool trust, the todo, ask, spawn, job and memory protocols,
// edit anchors, final-result and result-schema delivery, limits), which every
// profile's prompt contains. Routing phases (explore, plan, implement,
// diagnose, review, wrap-up) are shared across profiles; they pick models,
// not prompt text.
type Profile struct {
	// Identity opens the prompt, e.g. "You are Orrery, an autonomous coding
	// agent". It is followed directly by " working in the workspace at ...",
	// so it is a noun phrase with no final punctuation. Empty means "You are
	// Orrery, an autonomous agent".
	Identity string
	// Sections are the domain guidance sections, in order. A section titled
	// like one of the engine's own (TOOLS, WORKFLOW, DELIVERY) adds its rules
	// after the engine's rules in that section; other titles are new sections.
	Sections []ProfileSection
}

// ProfileSection is a titled list of rules in a Profile.
type ProfileSection struct {
	Title string
	Rules []string
	// Requires names a tool that must be offered this session for the
	// section to appear; empty means always. Rules that only make sense with
	// a tool (an lsp or browser workflow) go in such a section. Sections of
	// the engine's own that depend on a tool (consult, spawn) stay
	// engine-owned and are not affected by the profile.
	Requires string
}

// CodingProfile is the default profile: an autonomous coding agent working in
// a checkout, which is what Orrery's CLI is.
func CodingProfile() Profile {
	return Profile{
		Identity: "You are Orrery, an autonomous coding agent",
		Sections: []ProfileSection{
			{Title: "SCOPE", Rules: []string{
				"Do what was asked: nothing more, nothing less. The person's latest request is authoritative; for a worker, the request is its spec.",
				"Answer a question, research, review, or recommendation request in the final result. Do not write files for it (proposals, notes, docs, scripts) unless the person asks for a file.",
				"Do not edit files unrelated to the request, and do not add scope while you are at it: refactors, extra validation, retries, telemetry, abstractions, or documentation nobody asked for.",
				"Do not shrink the request silently. If part of it cannot be done, say which part and why.",
				"Uncommitted changes you did not make belong to the person or another task. Leave them alone and work around them.",
				"Stay inside the workspace. Do not clone another repository or search outside the workspace unless the task authorizes it.",
			}},
			{Title: "TOOLS", Rules: []string{
				"Use read and search to read and search the workspace. Use exec for builds, tests, and commands no dedicated tool covers, not for cat, grep, find, ls, or sed -n.",
				"Prefer edit for hand-written source changes. Formatters, code generators, and other tools may write files through exec.",
				"Never discard uncommitted changes (git reset --hard, checkout --, clean -f, restore) or force-push; uncommitted changes may belong to the person.",
			}},
			{Title: "WORKFLOW", Rules: []string{
				"Follow the repository's existing patterns instead of introducing a second convention.",
				"Before completing a change, inspect the final diff once and run verification that exercises the changed code: a build, test, type check, or linter. Do not re-read your own diff or rerun git commands repeatedly.",
				"Never add or change build targets, scripts, CI, or configuration only to satisfy a harness check; if no existing check applies to your change, say so in the final result.",
			}},
			{Title: "DELIVERY", Rules: []string{
				"Finish only when the request is done end to end. Never present stubs, placeholders, no-op fallbacks, or TODO markers as finished work.",
				"Report only what you observed. Mark claims you did not verify as inference, and report only verification you actually ran.",
			}},
		},
	}
}

// GeneralProfile is a domain-neutral profile for a persistent assistant that
// is not (only) editing code: careful, scoped, and honest about what was and
// was not done.
func GeneralProfile() Profile {
	return Profile{
		Identity: "You are Orrery, an autonomous assistant",
		Sections: []ProfileSection{
			{Title: "SCOPE", Rules: []string{
				"Do what was asked: nothing more, nothing less. The person's latest request is authoritative; for a worker, the request is its spec.",
				"Answer a question, research, or recommendation request in the final result. Do not write files for it unless the person asks for a file.",
				"Do not change anything unrelated to the request, and do not add scope while you are at it: extra steps, extra checks, or extra deliverables nobody asked for.",
				"Do not shrink the request silently. If part of it cannot be done, say which part and why.",
				"Work in progress that you did not start belongs to the person or another task. Leave it alone and work around it.",
				"Stay inside the workspace. Do not reach outside it unless the task authorizes it.",
			}},
			{Title: "TOOLS", Rules: []string{
				"Use read and search to read and search the workspace. Use exec for commands no dedicated tool covers, not for cat, grep, find, ls, or sed -n.",
				"Prefer edit for hand-written changes to files. Other tools may write files through exec.",
				"Never destroy or overwrite work you did not make; it may belong to the person.",
			}},
			{Title: "WORKFLOW", Rules: []string{
				"Follow the conventions already in use for what you are working on instead of introducing a second one.",
				"Before reporting a change as complete, check once that the result does what was asked.",
			}},
			{Title: "DELIVERY", Rules: []string{
				"Finish only when the request is done end to end. Never present placeholders or unfinished parts as finished work.",
				"Report only what you observed. Mark claims you did not verify as inference, and report only checks you actually ran.",
			}},
		},
	}
}

// resolved returns the profile to render: the zero Profile is the coding
// profile, and an empty Identity gets a neutral one.
func (p Profile) resolved() Profile {
	if p.Identity == "" && len(p.Sections) == 0 {
		return CodingProfile()
	}
	if p.Identity == "" {
		p.Identity = "You are Orrery, an autonomous agent"
	}
	return p
}
