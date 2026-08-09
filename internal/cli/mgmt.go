package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"bunk/internal/daemon"
)

func pidPath() string  { return filepath.Join(Home(), "daemon.pid") }
func logPath() string  { return filepath.Join(Home(), "daemon.log") }
func shimDir() string  { return filepath.Join(Home(), "bin") }
func shimPath() string { return filepath.Join(shimDir(), "docker") }

// StartDaemon spawns the background daemon.
func StartDaemon() error {
	if running, _ := daemonRunning(); running {
		return errors.New("daemon already running")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.MkdirAll(Home(), 0o755)
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		return err
	}
	// Give it a moment to open its control socket.
	time.Sleep(300 * time.Millisecond)
	fmt.Println("bunk daemon started (logs:", logPath()+")")
	return nil
}

// StopDaemon stops the background daemon.
func StopDaemon() error {
	pid, err := readPid()
	if err != nil {
		return errors.New("daemon not running")
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	_ = os.Remove(pidPath())
	fmt.Println("bunk daemon stopped")
	return nil
}

// RestartDaemon restarts the background daemon.
func RestartDaemon() error {
	_ = StopDaemon()
	time.Sleep(200 * time.Millisecond)
	return StartDaemon()
}

func readPid() (int, error) {
	raw, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

func daemonRunning() (bool, error) {
	pid, err := readPid()
	if err != nil {
		return false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	return proc.Signal(syscall.Signal(0)) == nil, nil
}

// EnableShim installs a `docker` shim that routes to the active machine.
func EnableShim() error {
	_ = os.MkdirAll(shimDir(), 0o755)
	self, err := os.Executable()
	if err != nil {
		self = "bunk"
	}
	script := "#!/bin/sh\n# bunk shim: routes docker to the active bunk machine\nexec \"" + self + "\" docker-shim \"$@\"\n"
	if err := os.WriteFile(shimPath(), []byte(script), 0o755); err != nil {
		return err
	}
	// Best-effort: also drop a symlink into ~/.local/bin if present.
	if lh, err := os.UserHomeDir(); err == nil {
		lb := filepath.Join(lh, ".local", "bin")
		if st, err := os.Stat(lb); err == nil && st.IsDir() {
			_ = os.Symlink(shimPath(), filepath.Join(lb, "docker"))
			fmt.Println("linked docker shim into ~/.local/bin (already on most PATHs)")
		}
	}
	fmt.Println("docker shim installed at", shimPath())
	fmt.Println("add to PATH if needed:  export PATH=\"$HOME/.bunk/bin:$PATH\"")
	fmt.Println("docker now runs on the active bunk machine; 'bunk unuse' goes back to local.")
	return nil
}

// DisableShim removes the docker shim.
func DisableShim() error {
	if err := os.Remove(shimPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if lh, err := os.UserHomeDir(); err == nil {
		_ = os.Remove(filepath.Join(lh, ".local", "bin", "docker"))
	}
	fmt.Println("docker shim removed")
	return nil
}

// InstallIdleGate enables the pause-when-active add-on in config.
func InstallIdleGate() error {
	path := filepath.Join(Home(), "config.yaml")
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	c := daemon.DefaultConfig()
	if raw, err := os.ReadFile(path); err == nil {
		_ = yaml.Unmarshal(raw, &c)
	}
	c.IdleGate.Enabled = true
	if c.IdleGate.ThresholdMs == 0 {
		c.IdleGate.ThresholdMs = 30000
	}
	raw, err := yaml.Marshal(&c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	fmt.Println("idle-gate enabled (needs Linux + X11 + xprintidle; restart the daemon: bunk restart)")
	fmt.Println("label containers to gate:  docker run -l bunk.idle-gate=1 ...")
	return nil
}
