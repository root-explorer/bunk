package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"bunk/internal/caps"
	"bunk/internal/proto"
)

// controlLoop serves the local CLI over a localhost TCP socket.
func (d *Daemon) controlLoop() {
	for {
		conn, err := d.ctrlLn.Accept()
		if err != nil {
			return
		}
		go d.handleControlConn(conn)
	}
}

func (d *Daemon) handleControlConn(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		var req proto.CtlReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			d.ctlReply(conn, proto.CtlResp{OK: false, Error: "bad request"})
			continue
		}
		resp := d.execControl(req)
		d.ctlReply(conn, resp)
		if resp.OK && req.Cmd == proto.CtlUse {
			// one command per connection for interactive simplicity is
			// unnecessary; keep the connection for repeated use.
		}
	}
}

func (d *Daemon) ctlReply(conn net.Conn, r proto.CtlResp) {
	raw, _ := json.Marshal(r)
	conn.Write(append(raw, '\n'))
}

// execControl runs one control command.
func (d *Daemon) execControl(req proto.CtlReq) proto.CtlResp {
	switch req.Cmd {
	case proto.CtlStatus:
		return d.ctlStatus()
	case proto.CtlPair:
		return d.ctlPair(req)
	case proto.CtlMachines:
		return d.ctlMachines()
	case proto.CtlRevoke:
		return d.ctlRevoke(req)
	case proto.CtlUse:
		return d.ctlUse(req)
	case proto.CtlUnuse:
		return d.ctlUnuse()
	case proto.CtlForward:
		return d.ctlForward(req)
	case proto.CtlUnforward:
		return d.ctlUnforward(req)
	case proto.CtlForwards:
		return d.ctlForwards()
	default:
		return proto.CtlResp{OK: false, Error: "unknown command " + req.Cmd}
	}
}

func (d *Daemon) ctlStatus() proto.CtlResp {
	d.mu.Lock()
	defer d.mu.Unlock()
	return proto.CtlResp{
		OK:        true,
		MachineID: d.st.MachineID,
		Name:      d.st.Name,
		Active:    d.st.Active,
		GPU:       caps.Detect().GPU,
		Machines:  d.machineListLocked(),
	}
}

func (d *Daemon) ctlMachines() proto.CtlResp {
	d.mu.Lock()
	defer d.mu.Unlock()
	return proto.CtlResp{OK: true, Machines: d.machineListLocked()}
}

func (d *Daemon) machineListLocked() []proto.MachineInfo {
	out := make([]proto.MachineInfo, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p)
	}
	return out
}

func (d *Daemon) ctlPair(req proto.CtlReq) proto.CtlResp {
	if req.Name != "" {
		d.mu.Lock()
		d.st.Name = req.Name
		d.mu.Unlock()
		d.saveState()
		// Re-hello so the hub records the new name.
		d.sendMsg(proto.Msg{
			Type:      proto.THello,
			MachineID: d.st.MachineID,
			Name:      d.st.Name,
			PubKey:    d.st.Key.PubB64,
			Token:     d.Cfg.Token,
		})
	}
	if req.Code == "" {
		// Host side: create a one-time code.
		d.sendMsg(proto.Msg{Type: proto.TCreateCode})
		code, err := d.awaitCode()
		if err != nil {
			return proto.CtlResp{OK: false, Error: err.Error()}
		}
		return proto.CtlResp{OK: true, Code: code, Name: d.st.Name}
	}
	// Guest side: redeem the code.
	d.sendMsg(proto.Msg{
		Type:      proto.TPair,
		Code:      req.Code,
		MachineID: d.st.MachineID,
		Name:      d.st.Name,
		PubKey:    d.st.Key.PubB64,
	})
	err := d.awaitPaired()
	if err != nil {
		return proto.CtlResp{OK: false, Error: err.Error()}
	}
	return proto.CtlResp{OK: true, Name: d.st.Name}
}

func (d *Daemon) ctlRevoke(req proto.CtlReq) proto.CtlResp {
	id, err := d.findPeerID(req.Name)
	if err != nil {
		return proto.CtlResp{OK: false, Error: err.Error()}
	}
	d.sendMsg(proto.Msg{Type: proto.TRevoke, Target: id})
	return proto.CtlResp{OK: true}
}

func (d *Daemon) ctlUse(req proto.CtlReq) proto.CtlResp {
	if req.Name == "" {
		return proto.CtlResp{OK: false, Error: "usage: bunk use <machine>"}
	}
	port, gpu, err := d.EnsureLink(req.Name)
	if err != nil {
		return proto.CtlResp{OK: false, Error: err.Error()}
	}
	d.mu.Lock()
	d.st.Active = req.Name
	d.mu.Unlock()
	d.saveState()
	return proto.CtlResp{OK: true, Port: port, GPU: gpu, Active: req.Name}
}

func (d *Daemon) ctlUnuse() proto.CtlResp {
	d.mu.Lock()
	d.st.Active = ""
	d.mu.Unlock()
	d.saveState()
	return proto.CtlResp{OK: true}
}

func (d *Daemon) ctlForward(req proto.CtlReq) proto.CtlResp {
	peer := d.st.Active
	if req.Name != "" {
		peer = req.Name
	}
	if peer == "" {
		return proto.CtlResp{OK: false, Error: "no machine selected (bunk use <machine>) or pass --machine"}
	}
	remote := req.Remote
	if remote == 0 {
		remote = req.Local
	}
	if err := d.EnsureForward(peer, req.Local, remote, req.Auto); err != nil {
		return proto.CtlResp{OK: false, Error: err.Error()}
	}
	return proto.CtlResp{OK: true, Port: req.Local}
}

func (d *Daemon) ctlUnforward(req proto.CtlReq) proto.CtlResp {
	if err := d.StopForward(req.Local); err != nil {
		return proto.CtlResp{OK: false, Error: err.Error()}
	}
	return proto.CtlResp{OK: true}
}

func (d *Daemon) ctlForwards() proto.CtlResp {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]proto.ForwardInfo, 0, len(d.st.Forwards))
	for _, f := range d.st.Forwards {
		out = append(out, *f)
	}
	return proto.CtlResp{OK: true, Forwards: out}
}

// --- async hub replies ----------------------------------------------------

func (d *Daemon) awaitCode() (string, error) {
	ch := make(chan string, 1)
	errCh := make(chan string, 1)
	d.mu.Lock()
	d.pendingCode = ch
	d.pendingCodeErr = errCh
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.pendingCode = nil
		d.pendingCodeErr = nil
		d.mu.Unlock()
	}()
	select {
	case c := <-ch:
		return c, nil
	case e := <-errCh:
		return "", fmt.Errorf("hub: %s", e)
	case <-time.After(15 * time.Second):
		return "", errors.New("timed out waiting for hub (is the hub reachable and the daemon connected?)")
	case <-d.stop:
		return "", fmt.Errorf("daemon stopped")
	}
}

func (d *Daemon) awaitPaired() error {
	ch := make(chan error, 1)
	d.mu.Lock()
	d.pendingPair = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.pendingPair = nil
		d.mu.Unlock()
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("timed out waiting for hub (is the hub reachable and the daemon connected?)")
	case <-d.stop:
		return fmt.Errorf("daemon stopped")
	}
}

// handleHubMsgExtras routes code/pair/error replies to waiters.
func (d *Daemon) handleHubMsgExtras(m proto.Msg) {
	switch m.Type {
	case proto.TCodeCreated:
		d.mu.Lock()
		if d.pendingCode != nil {
			d.pendingCode <- m.Code
		}
		d.mu.Unlock()
	case proto.TPaired:
		d.mu.Lock()
		if d.pendingPair != nil {
			d.pendingPair <- nil
		}
		d.mu.Unlock()
	case proto.TError:
		d.mu.Lock()
		if d.pendingCode != nil {
			d.pendingCodeErr <- m.Message
		}
		if d.pendingPair != nil {
			d.pendingPair <- fmt.Errorf("%s", m.Message)
		}
		d.mu.Unlock()
	}
}
