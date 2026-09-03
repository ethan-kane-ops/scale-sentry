package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/report"
)

func init() {
	rootCmd.AddCommand(newReportCmd())
}

func newReportCmd() *cobra.Command {
	var file string
	var plain bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render a diagnostic dashboard from a ScaleValidation status.diagnostics JSON array",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := readDiagnostics(file)
			if err != nil {
				return err
			}
			var alerts []v1beta1.DiagnosticAlert
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &alerts); err != nil {
					return fmt.Errorf("decode diagnostics json: %w", err)
				}
			}
			if plain {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), report.Render(alerts))
				return err
			}
			_, err = tea.NewProgram(report.NewModel(alerts)).Run()
			return err
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to a JSON array of diagnostic alerts (default: stdin)")
	cmd.Flags().BoolVar(&plain, "plain", false, "print the static dashboard instead of launching the interactive viewer")
	return cmd
}

func readDiagnostics(file string) ([]byte, error) {
	if file == "" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read diagnostics from stdin: %w", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read diagnostics file: %w", err)
	}
	return raw, nil
}
