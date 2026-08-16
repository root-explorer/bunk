package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"bunk/internal/daemon"
	"bunk/internal/proto"
)

// activeLink returns the docker proxy host for the active machine.
func activeLink() (string, string, error) {
	st, err := readState()
	if err != nil {
		return "", "", err
	}
	if st.Active == "" {
		return "", "", errors.New("no active machine — run: bunk use <machine>")
	}
	ln, ok := st.Links[st.Active]
	if !ok || ln.Port == 0 {
		return "", "", errors.New("no proxy for active machine — run: bunk use <machine>")
	}
	return fmt.Sprintf("tcp://127.0.0.1:%d", ln.Port), st.Active, nil
}

// loadCfg reads the daemon config for defaults (limits, gpu mode).
func loadCfg() daemon.Config {
	c := daemon.DefaultConfig()
	raw, err := os.ReadFile(filepath.Join(Home(), "config.yaml"))
	if err != nil {
		return c
	}
	_ = yaml.Unmarshal(raw, &c)
	return c
}

// DockerPassthrough runs docker on the active machine with safe defaults.
// Returns the exit code.
func DockerPassthrough(args []string, dryRun bool) int {
	host, active, err := activeLink()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bunk:", err)
		return 2
	}
	cfg := loadCfg()
	showCmd := os.Getenv("BUNK_SHOW_CMD") == "1"

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	var publishes []string
	detached := false
	if sub == "run" {
		var has map[string]bool
		has, publishes, detached = scanRunArgs(args[1:])
		args = injectDefaults(args, has, cfg, linkGPU(active))
	}

	full := append([]string{"--host", host}, args...)
	if dryRun || showCmd {
		fmt.Fprintf(os.Stderr, "bunk: %s\n", strings.Join(full, " "))
	}
	if dryRun {
		return 0
	}

	// Auto port-forward for docker run -p host:container mappings.
	// Disable with BUNK_NO_AUTO_FORWARD=1 (e.g. for manual management).
	var started []int
	if os.Getenv("BUNK_NO_AUTO_FORWARD") == "" {
		started = startAutoForwards(publishes, active)
	}
	defer stopAutoForwards(started, detached)

	return execDocker(full)
}

// DockerShim is used by the installed `docker` shim: when a machine is
// active it routes through the exact same pipeline as `bunk run` (limits,
// GPU auto-detect, auto port-forward); otherwise local docker, unchanged.
func DockerShim(args []string) int {
	if _, _, err := activeLink(); err != nil {
		return execDocker(args)
	}
	return DockerPassthrough(args, false)
}

func linkGPU(name string) string {
	st, err := readState()
	if err != nil {
		return "none"
	}
	if ln, ok := st.Links[name]; ok {
		return ln.GPU
	}
	return "none"
}

// injectDefaults adds limits + GPU flags only when the user didn't.
func injectDefaults(args []string, has map[string]bool, cfg daemon.Config, gpu string) []string {
	var extra []string
	if !has["--cpus"] && !has["--cpu-shares"] && cfg.Limits.CPUs > 0 {
		extra = append(extra, "--cpus", strconv.Itoa(cfg.Limits.CPUs))
	}
	if !has["--memory"] && !has["-m"] && cfg.Limits.MemoryGB > 0 {
		extra = append(extra, "--memory", fmt.Sprintf("%dg", cfg.Limits.MemoryGB))
	}
	if !has["--pids-limit"] && cfg.Limits.Pids > 0 {
		extra = append(extra, "--pids-limit", strconv.Itoa(cfg.Limits.Pids))
	}
	if cfg.GPU != "off" && os.Getenv("BUNK_GPU") != "off" {
		if gpu == "nvidia" && !has["--gpus"] && !has["--device"] {
			extra = append(extra, "--gpus", "all")
		}
	}
	if len(extra) == 0 {
		return args
	}
	// docker run <extra> ...
	out := append([]string{}, args[0])
	out = append(out, extra...)
	out = append(out, args[1:]...)
	return out
}

// scanRunArgs inspects docker run args up to the image name.
func scanRunArgs(args []string) (map[string]bool, []string, bool) {
	has := map[string]bool{}
	var publishes []string
	detached := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "" || a[0] != '-' {
			break // image name: stop scanning
		}
		if strings.HasPrefix(a, "--") {
			name, val := a, ""
			if eq := strings.Index(a, "="); eq >= 0 {
				name, val = a[:eq], a[eq+1:]
			}
			has[name] = true
			if name == "--detach" {
				detached = true
			}
			if name == "--publish" {
				publishes = append(publishes, val)
			}
			if val == "" && takesValue(name) {
				i++ // consume the value token
			}
			continue
		}
		// short flags, possibly combined (-dit, -p5432:5432)
		body := a[1:]
		for j := 0; j < len(body); j++ {
			f := "-" + string(body[j])
			has[f] = true
			if f == "-d" {
				detached = true
			}
			if f == "-p" {
				rest := body[j+1:]
				if rest != "" {
					publishes = append(publishes, rest)
					j = len(body)
				} else if i+1 < len(args) {
					i++
					publishes = append(publishes, args[i])
				}
			}
		}
	}
	return has, publishes, detached
}

func takesValue(flag string) bool {
	switch flag {
	case "--publish", "--cpus", "--memory", "--pids-limit", "--gpus", "--device",
		"--cpuset-cpus", "--name", "--env", "--env-file", "--volume", "--mount",
		"--network", "--workdir", "--user", "--platform", "--pull", "--restart",
		"--label", "--add-host", "--entrypoint", "--ulimit", "--tmpfs", "--ip",
		"--ip6", "--link", "--mac-address", "--health-cmd", "--log-driver",
		"--log-opt", "--storage-opt", "--expose", "--shm-size", "--cgroup-parent",
		"--cpu-shares", "--cpu-quota", "--cpu-period", "--blkio-weight",
		"--oom-score-adj", "--init-path", "--stop-signal", "--stop-timeout":
		return true
	}
	return false
}

func parsePublishHostPort(spec string) (int, bool) {
	parts := strings.Split(spec, ":")
	var host string
	switch len(parts) {
	case 2: // host:container
		host = parts[0]
	case 3: // ip:host:container
		host = parts[1]
	default: // container only -> docker picks a random host port
		return 0, false
	}
	if host == "" {
		return 0, false
	}
	p, err := strconv.Atoi(host)
	if err != nil {
		return 0, false
	}
	return p, true
}

// startAutoForwards opens local ports for explicit -p host mappings.
func startAutoForwards(publishes []string, machine string) []int {
	var started []int
	for _, spec := range publishes {
		hostPort, ok := parsePublishHostPort(spec)
		if !ok {
			continue
		}
		resp, err := ctl(proto.CtlReq{Cmd: proto.CtlForward, Name: machine, Local: hostPort, Remote: hostPort, Auto: true})
		if err != nil {
			fmt.Fprintf(os.Stderr, "bunk: auto-forward %d: %v\n", hostPort, err)
			continue
		}
		started = append(started, resp.Port)
		fmt.Fprintf(os.Stderr, "bunk: forwarding 127.0.0.1:%d -> %s:%d\n", resp.Port, machine, hostPort)
	}
	return started
}

func stopAutoForwards(ports []int, detached bool) {
	if detached {
		return // container keeps running: forwards persist until 'bunk unforward'
	}
	for _, p := range ports {
		ctl(proto.CtlReq{Cmd: proto.CtlUnforward, Local: p})
	}
}

// realDockerPath finds the actual docker binary, skipping the bunk shim
// directory (or any symlink resolving to it) so the shim never recurses
// into itself.
func realDockerPath() (string, error) {
	shim := shimDir()
	shimReal, _ := filepath.EvalSymlinks(shim)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		d := filepath.Clean(dir)
		if d == filepath.Clean(shim) {
			continue
		}
		p := filepath.Join(dir, "docker")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			real, rerr := filepath.EvalSymlinks(p)
			if rerr == nil && shimReal != "" && filepath.Clean(real) == filepath.Clean(filepath.Join(shimReal, "docker")) {
				continue // a symlink into the shim
			}
			return p, nil
		}
	}
	return "", errors.New("docker not found on PATH (outside the bunk shim)")
}

// execDockerSilent runs docker discarding output; returns exit code.
func execDockerSilent(args []string) int {
	bin, err := realDockerPath()
	if err != nil {
		return 2
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DOCKER_TLS_VERIFY=")
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 2
	}
	return 0
}

// execDocker runs the real docker binary with inherited stdio.
func execDocker(args []string) int {
	bin, err := realDockerPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bunk: docker:", err)
		return 2
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "DOCKER_TLS_VERIFY=")
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "bunk: docker:", err)
		return 2
	}
	return 0
}
