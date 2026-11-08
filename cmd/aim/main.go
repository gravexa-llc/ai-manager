// Command aim is the ai-manager CLI: the worktree inventory and cleanup,
// aimd's launchd agent, a one-line status and Go.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: aim <command>

aim is the ai-manager CLI.

Commands:
  ls         list worktrees with their state and a safe-to-remove verdict
  prune      remove worktrees that are safe to remove (a dry run by default)
  up         install aimd as a launchd agent and start it (--print: show the plist)
  down       stop aimd and remove its launchd agent
  status     one line from aimd: what needs you, stale sources, GitHub budget
  go         bring a session to the front in iTerm2 or Terminal (aim go <session>)
  version    print the version
  help       print this help
`

// commands maps each subcommand to its implementation, each in its own
// file (up.go, down.go, status.go, ...).
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"up":     cmdUp,
	"down":   cmdDown,
	"status": cmdStatus,
	"ls":     cmdLs,
	"prune":  cmdPrune,
	"go":     cmdGo,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if cmd, ok := commands[args[0]]; ok {
		return cmd(args[1:], stdout, stderr)
	}
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "aim %s\n", version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "aim: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
