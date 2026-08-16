// Package proto defines the wire protocol between bunk clients and the
// bunk-hub relay, plus the local daemon control protocol.
//
// Hub protocol: JSON messages over a WebSocket. Binary payloads are
// end-to-end encrypted (nacl/box) and carried base64-encoded in Msg.Data.
// The hub is a blind relay: it routes sealed frames between authorized
// peers and can never read their contents.
package proto

// Message types exchanged with the hub.
const (
	// client -> hub
	THello      = "hello"       // register/authenticate session
	TCreateCode = "create_code" // host asks for a one-time pairing code
	TPair       = "pair"        // guest redeems a code
	TRevoke     = "revoke"      // host (or either side) cuts a peer
	TList       = "list"        // refresh machine list
	TRelay      = "relay"       // sealed frame addressed to a peer
	TRelayClose = "relay_close" // close a relay channel

	// hub -> client
	TWelcome       = "welcome"
	TCodeCreated   = "code_created"
	TPaired        = "paired"
	TRevoked       = "revoked"
	TMachineUpdate = "machine_update"
	TError         = "error"
)

// Relay services a peer can provide.
const (
	ServiceDocker = "docker" // forward to the local docker daemon socket
	ServiceTCP    = "tcp"    // forward to 127.0.0.1:<Dial> on the remote
	ServiceCaps   = "caps"   // reply with the remote machine's capabilities
)

// Msg is the hub wire message.
type Msg struct {
	Type string `json:"type"`

	// identity
	MachineID string `json:"machine_id,omitempty"`
	Name      string `json:"name,omitempty"`
	PubKey    string `json:"pubkey,omitempty"` // base64 X25519 public key
	Token     string `json:"token,omitempty"`  // hub shared secret

	// pairing
	Code   string `json:"code,omitempty"`
	Target string `json:"target,omitempty"` // machine id

	// relay
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Channel string `json:"channel,omitempty"`
	Service string `json:"service,omitempty"` // docker | tcp | caps
	Dial    string `json:"dial,omitempty"`    // "127.0.0.1:5432" for ServiceTCP
	Data    string `json:"data,omitempty"`    // base64 sealed frame

	// responses
	Machines []MachineInfo `json:"machines,omitempty"`
	Peer     *MachineInfo  `json:"peer,omitempty"`
	Message  string        `json:"message,omitempty"` // error/extra text
}

// MachineInfo describes one machine in the pool.
type MachineInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	PubKey string `json:"pubkey,omitempty"` // base64 public key, for E2E sealing
	Online bool   `json:"online"`
}

// Control protocol: newline-delimited JSON over a localhost TCP socket
// between the bunk CLI and the local bunk daemon.

// CtlCmd values.
const (
	CtlStatus    = "status"
	CtlPair      = "pair"
	CtlMachines  = "machines"
	CtlRevoke    = "revoke"
	CtlUse       = "use"
	CtlUnuse     = "unuse"
	CtlForward   = "forward"
	CtlUnforward = "unforward"
	CtlForwards  = "forwards"
	CtlActive    = "active"
)

// CtlReq is a request to the local daemon.
type CtlReq struct {
	Cmd    string `json:"cmd"`
	Name   string `json:"name,omitempty"`   // machine name or id
	Code   string `json:"code,omitempty"`   // pairing code
	Local  int    `json:"local,omitempty"`  // local port
	Remote int    `json:"remote,omitempty"` // remote port
	Auto   bool   `json:"auto,omitempty"`   // auto-forward tied to container lifetime
}

// CtlResp is the daemon's answer.
type CtlResp struct {
	OK        bool          `json:"ok"`
	Error     string        `json:"error,omitempty"`
	MachineID string        `json:"machine_id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Active    string        `json:"active,omitempty"`
	Port      int           `json:"port,omitempty"`
	Code      string        `json:"code,omitempty"`
	GPU       string        `json:"gpu,omitempty"`
	Machines  []MachineInfo `json:"machines,omitempty"`
	Forwards  []ForwardInfo `json:"forwards,omitempty"`
}

// ForwardInfo describes an active port forward.
type ForwardInfo struct {
	Local   int    `json:"local"`
	Remote  int    `json:"remote"`
	Machine string `json:"machine"`
	Auto    bool   `json:"auto,omitempty"` // closed when the container stops
}
