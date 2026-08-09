// Package cli implements the bunk command-line interface: the tiny control
// surface (pair/use/machines/forward/...) plus the transparent docker
// passthrough with safe defaults.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bunk/internal/proto"
)

// Home returns the bunk home directory (~/.bunk unless BUNK_HOME set).
func Home() string {
	if h := os.Getenv("BUNK_HOME"); h != "" {
		return h
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return ".bunk"
	}
	return filepath.Join(hd, ".bunk")
}

// StatePath is the daemon state file path.
func StatePath() string { return filepath.Join(Home(), "state.json") }

// daemonState is a mirror of the daemon's on-disk state (read by the CLI).
type daemonState struct {
	MachineID   string               `json:"machine_id"`
	Key         *keyPairJSON         `json:"key"`
	Name        string               `json:"name"`
	ControlPort int                  `json:"control_port"`
	Active      string               `json:"active"`
	Links       map[string]*linkJSON `json:"links"`
}

type keyPairJSON struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

type linkJSON struct {
	Port int    `json:"port"`
	GPU  string `json:"gpu"`
}

func readState() (*daemonState, error) {
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		return nil, fmt.Errorf("daemon not running (no state file). Start it with: bunk start")
	}
	var s daemonState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return &s, nil
}

// ctl sends one control request to the local daemon.
func ctl(req proto.CtlReq) (proto.CtlResp, error) {
	st, err := readState()
	if err != nil {
		return proto.CtlResp{}, err
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", st.ControlPort), 2*time.Second)
	if err != nil {
		return proto.CtlResp{}, fmt.Errorf("daemon not reachable (start it with: bunk start)")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	raw, _ := json.Marshal(req)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return proto.CtlResp{}, err
	}
	dec := json.NewDecoder(io.LimitReader(conn, 1<<20))
	var resp proto.CtlResp
	if err := dec.Decode(&resp); err != nil {
		return proto.CtlResp{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// --- control commands -----------------------------------------------------

// Pair hosts or completes pairing.
func Pair(code string, name string) error {
	req := proto.CtlReq{Cmd: proto.CtlPair, Code: code, Name: name}
	resp, err := ctl(req)
	if err != nil {
		return err
	}
	if code == "" {
		fmt.Printf("Pairing code for %s:\n\n    %s\n\nShare it with the other machine, then run:  bunk pair %s\n",
			resp.Name, resp.Code, resp.Code)
	} else {
		fmt.Printf("Paired! Now run:  bunk use <machine>\n")
	}
	return nil
}

// Machines lists linked machines.
func Machines() error {
	resp, err := ctl(proto.CtlReq{Cmd: proto.CtlMachines})
	if err != nil {
		return err
	}
	if len(resp.Machines) == 0 {
		fmt.Println("no machines yet — run 'bunk pair' on the machine you want to share")
		return nil
	}
	fmt.Printf("%-20s %-6s %s\n", "NAME", "STATUS", "GPU")
	for _, m := range resp.Machines {
		status := "offline"
		if m.Online {
			status = "online"
		}
		fmt.Printf("%-20s %-6s %s\n", m.Name, status, "")
	}
	return nil
}

// Revoke cuts the link to a machine.
func Revoke(name string) error {
	if _, err := ctl(proto.CtlReq{Cmd: proto.CtlRevoke, Name: name}); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", name)
	return nil
}

// Use targets a machine (docker commands now run there).
func Use(name string, unset bool) error {
	if unset {
		if _, err := ctl(proto.CtlReq{Cmd: proto.CtlUnuse}); err != nil {
			return err
		}
		fmt.Println("back to local docker")
		return nil
	}
	resp, err := ctl(proto.CtlReq{Cmd: proto.CtlUse, Name: name})
	if err != nil {
		return err
	}
	ctxName := "bunk-" + resp.Active
	createContext(ctxName, resp.Port)
	fmt.Printf("now using %s (docker proxy on 127.0.0.1:%d, gpu=%s)\n", resp.Active, resp.Port, resp.GPU)
	fmt.Printf("  docker --context %s ...   or plain docker via: bunk enable-shim\n", ctxName)
	return nil
}

// createContext best-effort: registers a docker context so GUI tools work.
func createContext(name string, port int) {
	// Only create if missing — keeps repeated `bunk use` quiet.
	if execDockerSilent([]string{"context", "inspect", name}) == 0 {
		return
	}
	_ = execDocker([]string{"context", "create", name,
		"--docker", fmt.Sprintf("host=tcp://127.0.0.1:%d", port)})
}

// Forward exposes a remote service port on localhost.
func Forward(spec string) error {
	local, remote, err := parsePortSpec(spec)
	if err != nil {
		return err
	}
	resp, err := ctl(proto.CtlReq{Cmd: proto.CtlForward, Local: local, Remote: remote})
	if err != nil {
		return err
	}
	fmt.Printf("forwarding 127.0.0.1:%d -> remote :%d (stop: bunk unforward %d)\n",
		resp.Port, remote, resp.Port)
	return nil
}

// Unforward closes a forward.
func Unforward(local int) error {
	if _, err := ctl(proto.CtlReq{Cmd: proto.CtlUnforward, Local: local}); err != nil {
		return err
	}
	fmt.Printf("closed forward on %d\n", local)
	return nil
}

// Forwards lists active forwards.
func Forwards() error {
	resp, err := ctl(proto.CtlReq{Cmd: proto.CtlForwards})
	if err != nil {
		return err
	}
	if len(resp.Forwards) == 0 {
		fmt.Println("no active forwards")
		return nil
	}
	for _, f := range resp.Forwards {
		fmt.Printf("127.0.0.1:%d -> %s:%d\n", f.Local, f.Machine, f.Remote)
	}
	return nil
}

// Status shows daemon and hub state.
func Status() error {
	st, err := readState()
	if err != nil {
		return err
	}
	resp, err := ctl(proto.CtlReq{Cmd: proto.CtlStatus})
	if err != nil {
		return err
	}
	fmt.Printf("machine:  %s (%s)\n", resp.Name, shortID(st.MachineID))
	fmt.Printf("active:   %s\n", orDash(resp.Active))
	fmt.Printf("gpu:      %s\n", resp.GPU)
	fmt.Printf("linked:   %d machine(s)\n", len(resp.Machines))
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parsePortSpec(spec string) (int, int, error) {
	var local, remote int
	if i := strings.Index(spec, ":"); i >= 0 {
		if _, err := fmt.Sscanf(spec[:i], "%d", &local); err != nil {
			return 0, 0, fmt.Errorf("bad local port %q", spec[:i])
		}
		if _, err := fmt.Sscanf(spec[i+1:], "%d", &remote); err != nil {
			return 0, 0, fmt.Errorf("bad remote port %q", spec[i+1:])
		}
	} else {
		if _, err := fmt.Sscanf(spec, "%d", &local); err != nil {
			return 0, 0, fmt.Errorf("bad port %q", spec)
		}
		remote = local
	}
	return local, remote, nil
}
