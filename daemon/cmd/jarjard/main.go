// Command jarjard is the single JarJar server binary. `jarjard serve` is the
// resident daemon; all other subcommands are short-lived processes
// (ARCHITECTURE.md §2).
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/akimbohh/jarjar/daemon/internal/api"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const defaultConfigPath = "/etc/jarjar/jarjard.toml"

func main() {
	api.Version = version
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	var code int
	switch sub {
	case "serve":
		code = cmdServe(args)
	case "worker":
		code = cmdWorker(args)
	case "modtool":
		// The model's registry window; prints JSON to stdout.
		code = modtool.RunCLI(args, os.Stdout, os.Stderr)
	case "init":
		code = cmdInit(args)
	case "import":
		code = cmdImport(args)
	case "invite":
		code = cmdInvite(args)
	case "rollback":
		code = cmdRollback(args)
	case "status":
		code = cmdStatus(args)
	case "version", "--version", "-v":
		fmt.Println("jarjard", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `jarjard — JarJar server daemon

usage:
  jarjard init    --description "..." [flags]     one-time setup: plan, provision, build, boot an optimized server
  jarjard serve   [--config PATH]                 run the resident daemon
  jarjard worker  --job ID [--config PATH]        run one pipeline job (spawned by serve)
  jarjard modtool <search|project|versions|packsearch|packversions|installed> ...   registry query (JSON)
  jarjard import  PACKFILE [--name NAME] [--config PATH]    import a .mrpack / CurseForge zip as version 1
  jarjard invite  [--role player|admin] [--config PATH]     mint an invite code
  jarjard rollback --to N [--config PATH]         roll the pack back to version N
  jarjard status  [--config PATH]                 show queue and server status
  jarjard version                                 print version
`)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
