// Package protocol defines the JSON messages exchanged between the device
// client and the Hermes device plugin over one WebSocket.
//
// The device sends Hello first. The server answers Welcome, or closes the
// socket with CloseUnauthorized. After that the server sends Exec and Kill,
// and the device answers each Exec with zero or more Output messages and
// exactly one Exit.
package protocol

// Version is the protocol version the client speaks.
const Version = 1

// Message types.
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeExec    = "exec"
	TypeKill    = "kill"
	TypeOutput  = "output"
	TypeExit    = "exit"
	TypePing    = "ping"
	TypePong    = "pong"
)

// CloseUnauthorized is the WebSocket close code the server uses when the
// device name or token is wrong. The client stops reconnecting on it.
const CloseUnauthorized = 4401

// ExitRefused is the exit code the device reports when it refuses a request,
// for example a cwd outside the allowed roots.
const ExitRefused = 126

// Envelope carries the type of a message so it can be decoded in two steps.
type Envelope struct {
	Type string `json:"type"`
}

// Hello is the first message from the device.
type Hello struct {
	Type     string   `json:"type"`
	Version  int      `json:"version"`
	Name     string   `json:"name"`
	Token    string   `json:"token"`
	Roots    []string `json:"roots"`
	TempDir  string   `json:"tmp_dir"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
}

// Welcome is the server's answer to a valid Hello.
type Welcome struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Exec asks the device to run Cmd with bash in Cwd.
type Exec struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Cmd     string  `json:"cmd"`
	Cwd     string  `json:"cwd"`
	Login   bool    `json:"login"`
	Timeout float64 `json:"timeout"`
	Stdin   *string `json:"stdin,omitempty"`
}

// Kill asks the device to stop the command started by the Exec with ID.
type Kill struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Output is a chunk of merged stdout and stderr.
type Output struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data"`
}

// Exit reports how a command ended. Error is set when the device refused
// or could not start the command.
type Exit struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Code   int    `json:"code"`
	Killed bool   `json:"killed,omitempty"`
	Error  string `json:"error,omitempty"`
}
