// Package caps detects a machine's capabilities (OS, arch, GPU, cores, RAM).
package caps

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Caps describes a machine's capabilities. Reported to peers on demand so
// the CLI can apply the right defaults (e.g. --gpus all for Nvidia hosts).
type Caps struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	GPU      string `json:"gpu"` // nvidia | amd | none
	Hostname string `json:"hostname"`
	Cores    int    `json:"cores"`
	RAMGB    int    `json:"ram_gb"`
}

// Detect probes the local machine.
func Detect() Caps {
	return Caps{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		GPU:      DetectGPU(),
		Hostname: hostname(),
		Cores:    runtime.NumCPU(),
		RAMGB:    ramGB(),
	}
}

// DetectGPU returns nvidia | amd | none based on which tool is present.
func DetectGPU() string {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		if out, err := exec.Command("nvidia-smi", "-L").Output(); err == nil && len(out) > 0 {
			return "nvidia"
		}
	}
	if _, err := exec.LookPath("rocm-smi"); err == nil {
		return "amd"
	}
	return "none"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func ramGB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					// Round up so sub-1GB hosts report 1.
					return int((kb + 1024*1024 - 1) / (1024 * 1024))
				}
			}
			return 0
		}
	}
	return 0
}
