// Package protocol defines the wire names shared by the device client,
// hermes-hub and the Hermes device plugin. docs/protocol.md is the
// normative description; this package and plugin/device/core.py must agree
// with it, and the contract tests check that they do.
package protocol

import "time"

// Version is the exec leg protocol version. Version 1 was the proof of
// concept (token in hello, no hub); a plugin accepts it only with its
// dev_static_tokens setting on.
const Version = 2

// Exec leg message types.
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeExec    = "exec"
	TypeKill    = "kill"
	TypeOutput  = "output"
	TypeExit    = "exit"
	TypePing    = "ping"
	TypePong    = "pong"
	// TypeResume is reserved for phase 3 and must not be sent.
	TypeResume = "resume"
)

// WebSocket close codes. The hub relays a backend's close code to the
// device unchanged.
const (
	// CloseUnauthorized: the hub refused the device token, or (dev mode)
	// the plugin refused the static token. The client stops and asks for
	// login.
	CloseUnauthorized = 4401
	// CloseAssertionRefused: the plugin or acp-host refused the hub
	// assertion. The client retries with backoff.
	CloseAssertionRefused = 4403
	// CloseReplaced: a newer connection of the same device took over. The
	// run daemon stops, since another daemon with the same token is live.
	CloseReplaced = 4409
	// CloseUnsupportedVersion: the peer does not speak this version. The
	// client stops.
	CloseUnsupportedVersion = 4426
	// CloseBackendUnavailable: the hub could not reach the backend. The
	// client retries with backoff.
	CloseBackendUnavailable = 4502
	// ClosePingTimeout: no pong within PingTimeout.
	ClosePingTimeout = 4408
	// CloseUnsupportedPlatform: the device runs an OS the plugin does not
	// serve (Windows). The client stops.
	CloseUnsupportedPlatform = 4427
)

// Exit codes the plugin and device use besides the command's own.
const (
	ExitRefused = 126
	ExitKilled  = 130
	ExitLost    = 255
)

// Timing on every leg.
const (
	PingInterval = 25 * time.Second
	PingTimeout  = 15 * time.Second
	HelloTimeout = 10 * time.Second
)

// Paths on the backend.
const (
	PluginExecPath = "/api/plugins/device/ws"
	ACPHostPath    = "/acp"
)

// Envelope carries the type of a message so it can be decoded in two steps.
type Envelope struct {
	Type string `json:"type"`
}

// Hello is the first message from the device.
type Hello struct {
	Type          string   `json:"type"`
	Version       int      `json:"version"`
	Roots         []string `json:"roots"`
	TempDir       string   `json:"tmp_dir"`
	Home          string   `json:"home"`
	Hostname      string   `json:"hostname"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Shell         string   `json:"shell"`
	ClientVersion string   `json:"client_version"`
	// Name and Token are sent only to a plugin in dev_static_tokens mode,
	// with no hub in between.
	Name  string `json:"name,omitempty"`
	Token string `json:"token,omitempty"`
}

// Welcome is the plugin's answer to a valid Hello.
type Welcome struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	DeviceID      string `json:"device_id"`
	Name          string `json:"name"`
	PingInterval  int    `json:"ping_interval"`
	PluginVersion string `json:"plugin_version"`
	HermesVersion string `json:"hermes_version"`
}

// Exec asks the device to run Cmd with the shell in Cwd.
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

// Ping and Pong are the application-level liveness check that crosses the
// hub. Pong echoes Ping's Seq.
type Ping struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
}
