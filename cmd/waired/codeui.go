package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/platform/browser"
	"github.com/waired-ai/waired-agent/internal/runtime/codeui"
)

const codeuiCmdLong = `Run the bundled OpenCode coding agent in your browser, on your real project,
as you (open / status / url / stop). Other local users and external clients
are blocked. Under sudo it re-runs as the invoking user so opencode edits the
project as you, not root.

Run 'waired codeui --help' for the full open/status/url/stop flag set.`

// newCodeuiCmd integrates `waired codeui` into the cobra tree while keeping
// its existing behavior intact. codeui has special semantics — a sudo hop to
// the invoking user, "flags without a subcommand" meaning `open`, and its own
// hand-written usage — so flag parsing is disabled here and the raw args are
// handed to runCodeUI, mirroring how the internal proxy verbs delegate.
func newCodeuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "codeui",
		Short:              "Run the bundled OpenCode coding agent in your browser (open / status / url / stop).",
		Long:               codeuiCmdLong,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCodeUI(args)
		},
	}
}

// runCodeUI implements `waired codeui …`: the user-side bundled OpenCode coding
// agent (#486). It runs `opencode serve` AS the invoking user, with the child
// cwd set to the user's real project, behind an authenticating proxy. See
// internal/runtime/codeui/launcher.go for the process model.
//
//	waired codeui open [--project DIR] [--bind loopback|overlay] [--auth token|basic]
//	waired codeui status [--json]
//	waired codeui stop
//	waired codeui url [--json]
//	waired codeui serve …            # internal: the detached host `open` spawns
func runCodeUI(args []string) error {
	// Under `sudo waired codeui …` re-run as the invoking user so opencode
	// edits the project as the user (not root) and reads the per-user state
	// dir. A real root login (no SUDO_USER) is left as-is.
	if user, ok := invokingSudoUser(); ok {
		return hopCodeUIToUser(user, args)
	}

	if len(args) == 0 {
		return runCodeUIOpen(nil)
	}
	switch args[0] {
	case "open":
		return runCodeUIOpen(args[1:])
	case "serve":
		return runCodeUIServe(args[1:])
	case "stop":
		return runCodeUIStop(args[1:])
	case "status":
		return runCodeUIStatus(args[1:])
	case "url":
		return runCodeUIURL(args[1:])
	case "-h", "--help", "help":
		printCodeUIUsage()
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			// `waired codeui --project X` (flags, no subcommand) == open.
			return runCodeUIOpen(args)
		}
		printCodeUIUsage()
		return fmt.Errorf("unknown codeui subcommand %q", args[0])
	}
}

func printCodeUIUsage() {
	fmt.Fprint(os.Stderr, `usage: waired codeui <command> [flags]

Run the bundled OpenCode coding agent in your browser, on your real project,
as you. Other local users and external clients are blocked.

Commands:
  open    Start (or reuse) the coding agent and open it in your browser.
          --project DIR   project to open (default: current directory)
          --bind MODE     loopback (default) | overlay (reachable by waired
                          peers) | an explicit host/IP
          --auth MODE     token (default; friction-free link) | basic
          --no-browser    print the URL instead of opening a browser
          --json          print machine-readable JSON
  status  Show whether the coding agent is running and its URL.
  url     Print a fresh access URL for the running coding agent.
  stop    Stop the coding agent.

On a headless server, run 'waired codeui open --no-browser' and forward the
port:  ssh -L 9480:127.0.0.1:9480 <host>  then open the printed URL.
`)
}

// codeuiCommonFlags registers the flags shared by open/serve and returns
// resolved Options (project defaults to the current directory).
func codeuiCommonFlags(fs *flag.FlagSet) *codeui.Options {
	opts := &codeui.Options{}
	fs.StringVar(&opts.Project, "project", "", "project directory to open (default: current directory)")
	fs.StringVar(&opts.Bind, "bind", codeui.BindLoopback, "loopback | overlay | explicit host/IP")
	fs.StringVar(&opts.Auth, "auth", codeui.AuthToken, "token | basic")
	fs.IntVar(&opts.Port, "port", codeui.DefaultCodeUIPort, "front-door (proxy) port")
	fs.StringVar(&opts.GatewayBaseURL, "gateway-base-url", "", "local gateway base URL (default http://127.0.0.1:9473)")
	fs.StringVar(&opts.MgmtBaseURL, "mgmt", "", "local management API base URL (default http://127.0.0.1:9476)")
	fs.StringVar(&opts.BaseDir, "base-dir", "", "per-user codeui dir (default under your state dir)")
	return opts
}

func resolveProject(opts *codeui.Options) error {
	if opts.Project != "" {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve project dir: %w", err)
	}
	opts.Project = wd
	return nil
}

func runCodeUIOpen(args []string) error {
	fs := flag.NewFlagSet("codeui open", flag.ContinueOnError)
	opts := codeuiCommonFlags(fs)
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveProject(opts); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if !*asJSON {
		fmt.Fprintln(os.Stderr, "Starting the coding agent (first run downloads opencode, ~55MB)…")
	}
	info, err := codeui.Open(ctx, *opts)
	if err != nil {
		return err
	}

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	printCodeUIInfo(info)

	if *noBrowser || !browser.HasDisplay() || info.Bind != codeui.BindLoopback {
		if !*noBrowser && info.Bind == codeui.BindLoopback {
			fmt.Fprintln(os.Stderr, "  (no graphical session detected — open the URL above)")
			printTunnelHint(info)
		}
		return nil
	}
	if err := browser.Open(info.URL()); err != nil {
		fmt.Fprintf(os.Stderr, "  (could not open a browser: %v — open the URL above)\n", err)
	}
	return nil
}

// runCodeUIServe is the long-running host spawned (detached) by `open`. It
// blocks until SIGTERM/SIGINT.
func runCodeUIServe(args []string) error {
	fs := flag.NewFlagSet("codeui serve", flag.ContinueOnError)
	opts := codeuiCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveProject(opts); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return codeui.Serve(ctx, *opts)
}

func runCodeUIStatus(args []string) error {
	fs := flag.NewFlagSet("codeui status", flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "per-user codeui dir (default under your state dir)")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info, running := codeui.Status(codeui.Options{BaseDir: *baseDir})
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"running": running, "info": info})
	}
	if !running {
		fmt.Println("coding agent: not running")
		return nil
	}
	fmt.Println("coding agent: running")
	printCodeUIInfo(info)
	return nil
}

func runCodeUIURL(args []string) error {
	fs := flag.NewFlagSet("codeui url", flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "per-user codeui dir (default under your state dir)")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info, running := codeui.Status(codeui.Options{BaseDir: *baseDir})
	if !running {
		return fmt.Errorf("coding agent is not running (start it with `waired codeui open`)")
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Println(info.URL())
	return nil
}

func runCodeUIStop(args []string) error {
	fs := flag.NewFlagSet("codeui stop", flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "per-user codeui dir (default under your state dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info, running := codeui.Status(codeui.Options{BaseDir: *baseDir})
	if err := codeui.Stop(codeui.Options{BaseDir: *baseDir}); err != nil {
		return err
	}
	if running {
		fmt.Printf("stopped coding agent (was serving %s)\n", info.Project)
	} else {
		fmt.Println("coding agent was not running")
	}
	return nil
}

// printCodeUIInfo writes the human-facing summary: the access URL, the project,
// and the credentials when basic auth is in use.
func printCodeUIInfo(info *codeui.RuntimeInfo) {
	fmt.Printf("  URL:     %s\n", info.URL())
	fmt.Fprintf(os.Stderr, "  Project: %s\n", info.Project)
	switch info.Bind {
	case codeui.BindLoopback:
		// loopback: nothing extra.
	default:
		fmt.Fprintf(os.Stderr, "  Bind:    %s (reachable by your waired network peers)\n", info.ProxyAddr)
	}
	if info.Auth == codeui.AuthBasic {
		fmt.Fprintf(os.Stderr, "  Sign in: user %q  password %q\n", info.BasicUser, info.BasicPass)
	}
}

func printTunnelHint(info *codeui.RuntimeInfo) {
	fmt.Fprintf(os.Stderr, "  Tunnel:  ssh -L %d:127.0.0.1:%d <this-host>   then open the URL\n",
		codeui.DefaultCodeUIPort, codeui.DefaultCodeUIPort)
	_ = info
}

// hopCodeUIToUser re-runs `waired codeui <args>` as username via runuser (or
// `sudo -u … -H`), so the agent runs as the invoking user under sudo. Mirrors
// runLinkAllAsUser; stdio streams through so the URL reaches the operator.
func hopCodeUIToUser(username string, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve waired binary: %w", err)
	}
	childArgs := append([]string{"codeui"}, args...)
	var argv []string
	if p, lookErr := exec.LookPath("runuser"); lookErr == nil {
		argv = append([]string{p, "-u", username, "--", self}, childArgs...)
	} else if p, lookErr := exec.LookPath("sudo"); lookErr == nil {
		argv = append([]string{p, "-u", username, "-H", "--", self}, childArgs...)
	} else {
		return fmt.Errorf("neither runuser nor sudo is available to switch to user %s", username)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = scrubbedChildEnv(os.Environ())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
