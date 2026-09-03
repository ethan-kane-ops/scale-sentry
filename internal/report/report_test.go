package report

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

func TestRender_Empty(t *testing.T) {
	out := Render(nil)
	if !strings.Contains(out, "all checks passed") {
		t.Errorf("empty render missing pass message:\n%s", out)
	}
}

func TestRender_IncludesAlertContent(t *testing.T) {
	alerts := []v1beta1.DiagnosticAlert{
		{Type: "CPUThrottling", Severity: "Critical", Message: "throttled hard", Recommendation: "raise the limit"},
		{Type: "MissingPDB", Severity: "Warning", Message: "no budget"},
	}
	out := Render(alerts)

	for _, want := range []string{
		"CPUThrottling", "MissingPDB",
		"throttled hard", "no budget",
		"raise the limit",
		"1 critical", "1 warning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRender_SeverityOrdering(t *testing.T) {
	alerts := []v1beta1.DiagnosticAlert{
		{Type: "InfoOne", Severity: "Info", Message: "i"},
		{Type: "CritOne", Severity: "Critical", Message: "c"},
		{Type: "WarnOne", Severity: "Warning", Message: "w"},
	}
	out := Render(alerts)
	cIdx := strings.Index(out, "CritOne")
	wIdx := strings.Index(out, "WarnOne")
	iIdx := strings.Index(out, "InfoOne")
	if cIdx >= wIdx || wIdx >= iIdx {
		t.Errorf("alerts not ordered Critical<Warning<Info: c=%d w=%d i=%d", cIdx, wIdx, iIdx)
	}
}

func TestModel_QuitsOnKey(t *testing.T) {
	m := NewModel(nil)
	if cmd := m.Init(); cmd != nil {
		t.Error("Init should return nil cmd")
	}

	quitKeys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEscape},
		{Type: tea.KeyCtrlC},
	}
	for _, k := range quitKeys {
		_, cmd := m.Update(k)
		if cmd == nil {
			t.Errorf("key %q did not produce a quit cmd", k.String())
		}
	}

	// A non-quit key leaves the model running.
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}); cmd != nil {
		t.Error("non-quit key should not quit")
	}
}

func TestModel_ViewDelegatesToRender(t *testing.T) {
	alerts := []v1beta1.DiagnosticAlert{
		{Type: "DNSNdotsHigh", Severity: "Warning", Message: "ndots"},
	}
	v := NewModel(alerts).View()
	if !strings.Contains(v, "DNSNdotsHigh") {
		t.Errorf("View missing alert content:\n%s", v)
	}
	if !strings.Contains(v, "press q to quit") {
		t.Errorf("View missing footer:\n%s", v)
	}
}
