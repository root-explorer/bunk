// bunk is a utility for running containers on machines you trust, from
// anywhere, using the tools you already know. See README.md.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"bunk/internal/cli"
	"bunk/internal/daemon"
)

const usage = `bunk — your containers bunk on a friend's machine

Usage:
  bunk pair [code] [--name NAME]   one-time linking (host: no code = create one)
  bunk use <machine> [--unset]     point docker at that machine (or go local)
  bunk machines                    list linked machines + status
  bunk forward <local>[:<remote>]  expose a remote service port on localhost
  bunk unforward <port>            close a forward
  bunk forwards                    list active forwards
  bunk revoke <machine>            cut a link immediately
  bunk status                      daemon + hub status
  bunk start | stop | restart      manage the background daemon
  bunk enable-shim | disable-shim  make plain 'docker' route to the active machine
  bunk install-idle-gate           pause containers while the owner is active
  bunk --dry-run run <image> ...   show the exact docker command

Anything else is docker:  bunk run/exec/logs/ps/stop/rm/cp/build/push/pull/compose ...
runs on the active machine with safe defaults (CPU/RAM/pids limits, GPU passthrough).
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		return
	}

	dryRun := false
	if args[0] == "--dry-run" {
		dryRun = true
		args = args[1:]
		if len(args) == 0 {
			fmt.Print(usage)
			return
		}
	}

	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "pair":
		code, name := "", ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--name":
				if i+1 < len(rest) {
					i++
					name = rest[i]
				}
			default:
				if !strings.HasPrefix(rest[i], "-") && code == "" {
					code = rest[i]
				}
			}
		}
		err = cli.Pair(code, name)
	case "use":
		unset := false
		name := ""
		for _, a := range rest {
			if a == "--unset" {
				unset = true
			} else if !strings.HasPrefix(a, "-") {
				name = a
			}
		}
		if !unset && name == "" {
			err = fmt.Errorf("usage: bunk use <machine>")
			break
		}
		err = cli.Use(name, unset)
	case "unuse":
		err = cli.Use("", true)
	case "machines":
		err = cli.Machines()
	case "forward":
		if len(rest) != 1 {
			err = fmt.Errorf("usage: bunk forward <local>[:<remote>]")
			break
		}
		err = cli.Forward(rest[0])
	case "unforward":
		if len(rest) != 1 {
			err = fmt.Errorf("usage: bunk unforward <port>")
			break
		}
		p, perr := strconv.Atoi(rest[0])
		if perr != nil {
			err = perr
			break
		}
		err = cli.Unforward(p)
	case "forwards":
		err = cli.Forwards()
	case "revoke":
		if len(rest) != 1 {
			err = fmt.Errorf("usage: bunk revoke <machine>")
			break
		}
		err = cli.Revoke(rest[0])
	case "status":
		err = cli.Status()
	case "daemon":
		err = runDaemon()
	case "start":
		err = cli.StartDaemon()
	case "stop":
		err = cli.StopDaemon()
	case "restart":
		err = cli.RestartDaemon()
	case "enable-shim":
		err = cli.EnableShim()
	case "disable-shim":
		err = cli.DisableShim()
	case "install-idle-gate":
		err = cli.InstallIdleGate()
	case "docker-shim":
		os.Exit(cli.DockerShim(rest))
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		os.Exit(cli.DockerPassthrough(args, dryRun))
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "bunk:", err)
		os.Exit(1)
	}
}

func runDaemon() error {
	d, err := daemon.New(cli.Home())
	if err != nil {
		return err
	}
	return d.Run()
}
