package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Table layout tuning for tabwriter.
const (
	tabMinWidth = 2
	tabPadding  = 2
)

// outputPreviewLen bounds preview columns so one large action output does
// not wreck the table.
const outputPreviewLen = 48

// tableTimeLayout renders timestamps compactly for table columns.
const tableTimeLayout = "2006-01-02 15:04:05"

// newTab builds the column writer every tabular command shares.
func newTab(out io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(out, tabMinWidth, 0, tabPadding, ' ', 0)
}

// tabWritef writes one table line with the same broken-stream tolerance as
// printOutf.
func (a *App) tabWritef(tab *tabwriter.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(tab, format, args...); err != nil {
		a.logger.Warn("write table", "error", err)
	}
}

// flushTab flushes a table, logging instead of failing: a flush error means
// the output stream itself is gone.
func (a *App) flushTab(tab *tabwriter.Writer) {
	if err := tab.Flush(); err != nil {
		a.logger.Warn("flush table", "error", err)
	}
}

// writeActionTable renders one row per action with status, cost, and a
// truncated output preview.
func (a *App) writeActionTable(results []*store.ActionResult) {
	tab := newTab(a.stdout)

	a.tabWritef(tab, "ACTION\tRUNNER\tSTATUS\tATTEMPT\tCOST\tOUTPUT\tERROR\n")

	for _, result := range results {
		var errText string

		if result.Error != nil {
			errText = result.Error.Code
		}

		var output string

		if result.Output != nil {
			output = truncate(fmt.Sprintf("%v", result.Output))
		}

		a.tabWritef(tab, "%s\t%s\t%s\t%d\t$%.4f\t%s\t%s\n",
			result.ActionName, result.Runner, result.Status, result.Attempt,
			result.Usage.CostUSD, output, errText)
	}

	a.flushTab(tab)
}

// formatTableTime renders a timestamp for a table, with a dash for the zero
// time so "not yet" stays readable. UTC keeps rows comparable with the
// store's own timestamps.
func formatTableTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}

	return value.UTC().Format(tableTimeLayout)
}

// truncate caps text at outputPreviewLen bytes for preview columns.
func truncate(text string) string {
	if len(text) <= outputPreviewLen {
		return text
	}

	return text[:outputPreviewLen] + "..."
}
