package daemon

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// runIdleGate pauses containers labeled bunk.idle-gate=1 while the host's
// owner is actively using the machine, and resumes them when idle again.
// Requires X11 + xprintidle on Linux (the "respect the host" add-on).
func (d *Daemon) runIdleGate() {
	if runtime.GOOS != "linux" {
		log.Printf("idle-gate: Linux-only for now; falling back to limits+agreement")
		return
	}
	if _, err := exec.LookPath("xprintidle"); err != nil {
		log.Printf("idle-gate: xprintidle not installed (apt install xprintidle); disabled")
		return
	}
	if os.Getenv("DISPLAY") == "" {
		log.Printf("idle-gate: DISPLAY not set (X11 required); disabled")
		return
	}
	threshold := d.Cfg.IdleGate.ThresholdMs
	if threshold <= 0 {
		threshold = 30000
	}
	interval := time.Duration(d.Cfg.IdleGate.IdleSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	log.Printf("idle-gate enabled: pausing bunk.idle-gate containers when user is active")
	paused := false
	for {
		select {
		case <-d.stop:
			return
		case <-time.After(interval):
		}
		idle, err := xprintidle()
		if err != nil {
			continue
		}
		active := idle < threshold
		if active && !paused {
			d.setPaused(true)
			paused = true
		}
		if !active && paused {
			d.setPaused(false)
			paused = false
		}
	}
}

func xprintidle() (int, error) {
	out, err := exec.Command("xprintidle").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// setPaused pauses/unpauses containers labeled bunk.idle-gate=1.
func (d *Daemon) setPaused(pause bool) {
	ids, err := dockerListIdleGate()
	if err != nil || len(ids) == 0 {
		return
	}
	verb := "unpause"
	if pause {
		verb = "pause"
	}
	for _, id := range ids {
		if err := exec.Command("docker", verb, id).Run(); err != nil {
			log.Printf("idle-gate %s %s: %v", verb, id, err)
		}
	}
}

func dockerListIdleGate() ([]string, error) {
	out, err := exec.Command("docker", "ps", "-q", "-f", "label=bunk.idle-gate=1").Output()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Fields(string(out)) {
		ids = append(ids, l)
	}
	return ids, nil
}
