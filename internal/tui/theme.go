package tui

import (
	"charm.land/lipgloss/v2"
)

// The palette mirrors the web UI (internal/web/static/index.html) so a
// session looks like the same product in a browser and a terminal.
var (
	colorBright    = lipgloss.Color("#e6e8eb")
	colorSignal    = lipgloss.Color("#9aa0a6")
	colorMuted     = lipgloss.Color("#6b7079")
	colorCircuit   = lipgloss.Color("#2a2e35")
	colorPanel     = lipgloss.Color("#16181c")
	colorElectric  = lipgloss.Color("#00ffcc")
	colorElectricD = lipgloss.Color("#00cc99")
	colorConductor = lipgloss.Color("#9b80f8")
	colorDanger    = lipgloss.Color("#ff7a85")
	colorWarning   = lipgloss.Color("#f6d86b")
)

type styles struct {
	bright, signal, muted, accent, accentDim, purple, danger, warning lipgloss.Style
	bold, italic                                                      lipgloss.Style
	userBar, userText                                                 lipgloss.Style
	toolName, toolArg, gutter                                         lipgloss.Style
	statusPass, statusFail                                            lipgloss.Style
	key                                                               lipgloss.Style
}

func newStyles() styles {
	base := lipgloss.NewStyle()
	return styles{
		bright:     base.Foreground(colorBright),
		signal:     base.Foreground(colorSignal),
		muted:      base.Foreground(colorMuted),
		accent:     base.Foreground(colorElectric),
		accentDim:  base.Foreground(colorElectricD),
		purple:     base.Foreground(colorConductor),
		danger:     base.Foreground(colorDanger),
		warning:    base.Foreground(colorWarning),
		bold:       base.Bold(true).Foreground(colorBright),
		italic:     base.Italic(true).Foreground(colorMuted),
		userBar:    base.Foreground(colorElectric).Background(colorPanel),
		userText:   base.Foreground(colorBright).Background(colorPanel),
		toolName:   base.Bold(true).Foreground(colorBright),
		toolArg:    base.Foreground(colorSignal),
		gutter:     base.Foreground(colorCircuit),
		statusPass: base.Bold(true).Foreground(colorElectric),
		statusFail: base.Bold(true).Foreground(colorDanger),
		key:        base.Foreground(colorSignal).Bold(true),
	}
}
