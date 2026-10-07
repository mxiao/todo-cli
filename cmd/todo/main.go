// Command todo is the todo-cli entry point.
package main

import (
	"os"

	"github.com/mxiao/todo-cli/packages/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.OSEnv()))
}
