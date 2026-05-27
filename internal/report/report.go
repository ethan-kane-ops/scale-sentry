// Package report renders a terminal dashboard of diagnostic alerts. The
// pure [Render] function produces the dashboard string (used in tests and
// for non-interactive output); [Model] wraps it in a Bubble Tea program
// for interactive viewing.
package report

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("63")).
			Padding(0, 1)
	criticalStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	warningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	infoStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	okStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// severityRank orders alerts Critical → Warning → Info → unknown.
func severityRank(s string) int {
	switch s {
	case "Critical":
		return 0
	case "Warning":
		return 1
	case "Info":
		return 2
	default:
		return 3
	}
}

func styleFor(severity string) lipgloss.Style {
	switch severity {
	case "Critical":
		return criticalStyle
	case "Warning":
		return warningStyle
	default:
		return infoStyle
	}
}

// Render produces the static dashboard string from a set of alerts.
// Safe to call without a TTY, Lip Gloss degrades to plain text when the
// output is not a terminal.
func Render(alerts []v1alpha1.DiagnosticAlert) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("scale-sentry · diagnostic report"))
	b.WriteString("\n\n")

	if len(alerts) == 0 {
		b.WriteString(okStyle.Render("✓ all checks passed, no diagnostics"))
		b.WriteString("\n")
		return b.String()
	}

	var crit, warn, info int
	for _, a := range alerts {
		switch a.Severity {
		case "Critical":
			crit++
		case "Warning":
			warn++
		default:
			info++
		}
	}
	fmt.Fprintf(&b, "%s   %s   %s\n\n",
		criticalStyle.Render(fmt.Sprintf("● %d critical", crit)),
		warningStyle.Render(fmt.Sprintf("● %d warning", warn)),
		infoStyle.Render(fmt.Sprintf("● %d info", info)),
	)

	// Stable sort: severity first, original order preserved within a band.
	ordered := make([]v1alpha1.DiagnosticAlert, len(alerts))
	copy(ordered, alerts)
	sort.SliceStable(ordered, func(i, j int) bool {
		return severityRank(ordered[i].Severity) < severityRank(ordered[j].Severity)
	})

	for _, a := range ordered {
		b.WriteString(renderAlert(a))
		b.WriteString("\n")
	}
	return b.String()
}

func renderAlert(a v1alpha1.DiagnosticAlert) string {
	style := styleFor(a.Severity)
	var b strings.Builder
	b.WriteString(style.Render(fmt.Sprintf("[%s] %s", strings.ToUpper(a.Severity), a.Type)))
	b.WriteString("\n  ")
	b.WriteString(a.Message)
	if a.Recommendation != "" {
		b.WriteString("\n  ")
		b.WriteString(dimStyle.Render("↳ " + a.Recommendation))
	}
	b.WriteString("\n")
	return b.String()
}

// Model is the Bubble Tea model wrapping the diagnostic dashboard. It is a
// read-only view: any key quits.
type Model struct {
	alerts []v1alpha1.DiagnosticAlert
}

// NewModel constructs a Model over the given alerts.
func NewModel(alerts []v1alpha1.DiagnosticAlert) Model {
	return Model{alerts: alerts}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Update implements tea.Model, quits on q, esc, or ctrl+c.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m Model) View() string {
	return Render(m.alerts) + "\n" + dimStyle.Render("press q to quit")
}
