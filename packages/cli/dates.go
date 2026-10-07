package cli

import "github.com/mxiao/todo-cli/packages/core"

// Due and filter times use the shared parser so the CLI, the web service and
// model-created tasks read "明天", "fri" or "+3d" identically.
var (
	parseWhen = core.ParseWhen
	parseDue  = core.ParseDue
	endOfDay  = core.EndOfDay
)
