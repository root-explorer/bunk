package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// --- tcp forward listener -------------------------------------------------

// EnsureForward opens 127.0.0.1:local -> remote 127.0.0.1:remote through
// the relay to peer.
func (d *Daemon) EnsureForward(peerNameOrID string, local, remote int) error {
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
	d.st.Forwards = append(d.st.Forwards, &proto.ForwardInfo{Local: local, Remote: remote, Machine: peer.Name})
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
	go d.pumpConnToPeer(c.id)
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
