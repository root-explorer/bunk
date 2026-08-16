package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"bunk/internal/caps"
	"bunk/internal/proto"
)

// --- docker proxy listener (consumer side: "bunk use <machine>") ----------

// EnsureLink returns the docker proxy port for a peer, creating the
// listener if needed. Also refreshes the cached GPU info.
func (d *Daemon) EnsureLink(nameOrID string) (int, string, error) {
	peer, err := d.resolvePeer(nameOrID)
	if err != nil {
		return 0, "", err
	}
	d.mu.Lock()
	existing, ok := d.st.Links[peer.Name]
	d.mu.Unlock()
	if ok && existing.Port > 0 {
		return existing.Port, existing.GPU, nil
	}

	// Probe the peer's GPU through the relay.
	gpu := "none"
	if c, err := d.waitCaps(peer.ID); err == nil {
		gpu = c.GPU
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	d.mu.Lock()
	d.linkLns[peer.Name] = ln
	d.st.Links[peer.Name] = &LinkInfo{Port: port, GPU: gpu}
	d.mu.Unlock()
	d.saveState()

	go d.acceptLink(ln, peer.ID)
	return port, gpu, nil
}

func (d *Daemon) acceptLink(ln net.Listener, peerID string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.pipeLocal(conn, peerID, proto.ServiceDocker, "")
	}
}

// restoreListeners recreates link and forward listeners that were
// persisted in state.json, so docker proxies survive daemon restarts
// (a stale port with no listener = "Cannot connect" for the CLI).
func (d *Daemon) restoreListeners() {
	d.mu.Lock()
	links := map[string]int{}
	for name, li := range d.st.Links {
		if li != nil && li.Port > 0 {
			links[name] = li.Port
		}
	}
	forwards := append([]*proto.ForwardInfo{}, d.st.Forwards...)
	d.mu.Unlock()

	for name, port := range links {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			log.Printf("restore link %s: %v", name, err)
			continue
		}
		d.mu.Lock()
		if _, exists := d.linkLns[name]; exists {
			d.mu.Unlock()
			ln.Close()
			continue
		}
		d.linkLns[name] = ln
		d.mu.Unlock()
		go d.acceptForName(ln, name, proto.ServiceDocker, "")
	}

	for _, f := range forwards {
		if f == nil || f.Local <= 0 {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.Local))
		if err != nil {
			log.Printf("restore forward %d: %v", f.Local, err)
			continue
		}
		d.mu.Lock()
		if _, exists := d.fwdLns[f.Local]; exists {
			d.mu.Unlock()
			ln.Close()
			continue
		}
		d.fwdLns[f.Local] = ln
		d.mu.Unlock()
		go d.acceptForName(ln, f.Machine, proto.ServiceTCP, fmt.Sprintf("127.0.0.1:%d", f.Remote))
	}
}

// acceptForName accepts on a restored listener and resolves the peer id
// by name at accept time (peers are known only after hub connect).
func (d *Daemon) acceptForName(ln net.Listener, name, service, dial string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		pid, ok := d.resolvePeerID(name)
		if !ok {
			for i := 0; i < 20; i++ { // wait up to ~10s for hub connect
				time.Sleep(500 * time.Millisecond)
				if pid, ok = d.resolvePeerID(name); ok {
					break
				}
			}
		}
		if !ok {
			conn.Close()
			continue
		}
		go d.pipeLocal(conn, pid, service, dial)
	}
}

// resolvePeerID finds a machine id by its display name.
func (d *Daemon) resolvePeerID(name string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.peers[name]; ok {
		return p.ID, true
	}
	for _, p := range d.peers {
		if p.Name == name {
			return p.ID, true
		}
	}
	return "", false
}

// --- tcp forward listener -------------------------------------------------

// EnsureForward opens 127.0.0.1:local -> remote 127.0.0.1:remote through
// the relay to peer. Auto forwards are closed by watchAutoForwards once no
// running container on the peer publishes the remote port.
func (d *Daemon) EnsureForward(peerNameOrID string, local, remote int, auto bool) error {
	if local <= 0 || local > 65535 || remote <= 0 || remote > 65535 {
		return errors.New("ports must be 1-65535")
	}
	peer, err := d.resolvePeer(peerNameOrID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if _, exists := d.fwdLns[local]; exists {
		d.mu.Unlock()
		return fmt.Errorf("port %d already forwarded (bunk unforward %d)", local, local)
	}
	d.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", local, err)
	}
	d.mu.Lock()
	d.fwdLns[local] = ln
	d.st.Forwards = append(d.st.Forwards, &proto.ForwardInfo{Local: local, Remote: remote, Machine: peer.Name, Auto: auto})
	d.mu.Unlock()
	d.saveState()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.pipeLocal(conn, peer.ID, proto.ServiceTCP, fmt.Sprintf("127.0.0.1:%d", remote))
		}
	}()
	return nil
}

// StopForward closes a forward listener.
func (d *Daemon) StopForward(local int) error {
	d.mu.Lock()
	ln, ok := d.fwdLns[local]
	if ok {
		delete(d.fwdLns, local)
	}
	kept := d.st.Forwards[:0]
	for _, f := range d.st.Forwards {
		if f.Local != local {
			kept = append(kept, f)
		}
	}
	d.st.Forwards = kept
	d.mu.Unlock()
	d.saveState()
	if !ok {
		return fmt.Errorf("no forward on port %d", local)
	}
	return ln.Close()
}

// pipeLocal pumps a locally-accepted conn through a relay channel.
func (d *Daemon) pipeLocal(conn net.Conn, peerID, service, dial string) {
	c, err := d.openChannel(peerID, service, dial)
	if err != nil {
		conn.Close()
		return
	}
	d.chMu.Lock()
	c.conn = conn
	d.chMu.Unlock()
	// Relay any bytes the peer sent before we attached the conn.
	if c.kind == "link" && len(c.caps) > 0 {
		conn.Write(c.caps)
		c.caps = nil
	}
	go d.pumpConnToPeer(c.id, false)
	// Wait until the channel closes, then close the local conn.
	go func() {
		<-c.done
		conn.Close()
	}()
}

// --- caps probe (consumer side) -------------------------------------------

// waitCaps asks the peer for its capabilities.
func (d *Daemon) waitCaps(peerID string) (*caps.Caps, error) {
	c, err := d.openChannel(peerID, proto.ServiceCaps, "")
	if err != nil {
		return nil, err
	}
	select {
	case <-c.done:
	case <-time.After(6 * time.Second):
		d.closeChannel(c.id, true)
		return nil, errors.New("caps probe timed out")
	}
	var out caps.Caps
	if err := json.Unmarshal(c.caps, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- peer resolution -------------------------------------------------------

// resolvePeer finds a peer by name or id.
func (d *Daemon) resolvePeer(nameOrID string) (proto.MachineInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.peers[nameOrID]; ok {
		return p, d.requireOnline(p)
	}
	var found []proto.MachineInfo
	for _, p := range d.peers {
		if p.Name == nameOrID {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return proto.MachineInfo{}, errors.New("unknown machine (run 'bunk machines')")
	}
	if len(found) > 1 {
		return proto.MachineInfo{}, errors.New("ambiguous name, use the machine id")
	}
	return found[0], d.requireOnline(found[0])
}

func (d *Daemon) requireOnline(p proto.MachineInfo) error {
	if !p.Online {
		return fmt.Errorf("%s is offline", p.Name)
	}
	return nil
}

// findPeerID resolves a name to an id for revocation (online not required).
func (d *Daemon) findPeerID(nameOrID string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.peers[nameOrID]; ok {
		return p.ID, nil
	}
	for _, p := range d.peers {
		if p.Name == nameOrID {
			return p.ID, nil
		}
	}
	return "", errors.New("unknown machine (run 'bunk machines')")
}

// --- auto-forward lifecycle (close on container stop) ---------------------

// watchAutoForwards periodically checks each auto forward: if no running
// container on the target machine publishes the remote port anymore, the
// forward is closed. Runs for the lifetime of the daemon.
func (d *Daemon) watchAutoForwards() {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for range t.C {
		d.mu.Lock()
		var autos []proto.ForwardInfo
		for _, f := range d.st.Forwards {
			if f.Auto {
				autos = append(autos, *f)
			}
		}
		d.mu.Unlock()
		if len(autos) == 0 {
			continue
		}
		byMachine := map[string][]proto.ForwardInfo{}
		for _, f := range autos {
			byMachine[f.Machine] = append(byMachine[f.Machine], f)
		}
		for machine, fwds := range byMachine {
			live, err := d.containerPublicPorts(machine)
			if err != nil {
				continue // link not ready: retry next tick
			}
			for _, f := range fwds {
				if !live[f.Remote] {
					log.Printf("auto-forward: closing %d -> %s:%d (container stopped)", f.Local, machine, f.Remote)
					d.StopForward(f.Local)
				}
			}
		}
	}
}

// containerPublicPorts returns the set of host ports published by running
// containers on the peer's docker daemon, queried through the link proxy.
func (d *Daemon) containerPublicPorts(machine string) (map[int]bool, error) {
	d.mu.Lock()
	lnk, ok := d.st.Links[machine]
	d.mu.Unlock()
	if !ok || lnk.Port == 0 {
		return nil, errors.New("no link")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", lnk.Port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /containers/json HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePublicPorts(data)
}

// parsePublicPorts extracts the set of published host ports from a docker
// /containers/json response body.
func parsePublicPorts(data []byte) (map[int]bool, error) {
	var list []struct {
		Ports []struct {
			PublicPort int `json:"PublicPort"`
		} `json:"Ports"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, c := range list {
		for _, p := range c.Ports {
			if p.PublicPort > 0 {
				out[p.PublicPort] = true
			}
		}
	}
	return out, nil
}
