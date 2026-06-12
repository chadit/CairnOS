package cli

import (
	"strings"

	"github.com/chadit/CairnOS/internal/workflow"
)

func (a *App) cmdValidate(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		a.printErrf("usage: cairnos validate <workflow.toml>\n")

		return ExitUsage
	}

	if _, err := workflow.Load(args[0]); err != nil {
		a.printWorkflowErrors(err)

		return ExitUsage
	}

	a.printOutf("valid\n")

	return ExitOK
}
