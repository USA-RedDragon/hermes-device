// Package protocol defines the wire names shared by the device client,
// hermes-hub and the Hermes device plugin. docs/protocol.md is the
// normative description; this package and plugin/device/core.py must agree
// with it, and the contract tests check that they do.
package protocol

import (
	"crypto/sha1" //nolint:gosec // see BrowserRedirectURL
	"encoding/hex"
	"encoding/json"
	"time"
)

// Version is the newest exec leg protocol version this module speaks, and
// MinVersion the oldest. Version 3 adds resume (docs/protocol.md section
// 4.7). Version 1 was the proof of concept (token in hello, no hub); a
// plugin accepts it only with its dev_static_tokens setting on.
const (
	Version    = 3
	MinVersion = 2
)

// ResumeVersion is the first version that keeps commands across a drop.
const ResumeVersion = 3

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
	// TypeResume is sent by the plugin right after a version 3 welcome.
	TypeResume = "resume"
	// TypeBrowser asks the device to run one browser operation
	// (docs/browser.md). The device answers with exactly one
	// TypeBrowserResult. A Kill with the same ID cancels it. Only on a
	// leg that negotiated CapabilityBrowser.
	TypeBrowser       = "browser"
	TypeBrowserResult = "browser_result"
	// TypeBrowserStatus is sent by the device after the welcome (and the
	// resume) and whenever the browser extension connects, goes away or
	// changes its sign-in. The hub rewrites it (docs/browser.md section 13).
	TypeBrowserStatus = "browser_status"
	// TypeBrowserAuth is sent by the device to the hub with the browser
	// credential the extension holds, after the welcome (and the resume)
	// and whenever it changes. The hub answers with TypeBrowserAuthResult
	// and never forwards it to the plugin.
	TypeBrowserAuth = "browser_auth"
	// TypeBrowserAuthResult is sent by the hub to the device: the answer
	// to a TypeBrowserAuth, or a sign-in the hub dropped later. The hub
	// drops any the plugin sends.
	TypeBrowserAuthResult = "browser_auth_result"
	// TypeMCP asks the device for one operation on an MCP server of its
	// config (section 4.9). The device answers with exactly one
	// TypeMCPResult. A Kill with the same ID cancels it. Only on a leg
	// that negotiated CapabilityMCP.
	TypeMCP       = "mcp"
	TypeMCPResult = "mcp_result"
)

// Capabilities a device offers in Hello.Capabilities. The plugin answers
// with the ones it accepts in Welcome.Capabilities, and both sides use only
// those. They add messages without a protocol version bump.
const (
	// CapabilityBrowser: the device serves TypeBrowser requests.
	CapabilityBrowser = "browser"
	// CapabilityMCP: the device serves TypeMCP requests for the servers
	// it names in Hello.MCPServers.
	CapabilityMCP = "mcp"
)

// Operations of an MCP request.
const (
	// MCPOpList returns the state of every server: {"servers": [...]}.
	MCPOpList = "list"
	// MCPOpTools returns the server's tools/list, every page joined:
	// {"tools": [...]}.
	MCPOpTools = "tools"
	// MCPOpCall runs tools/call and returns the server's CallToolResult.
	MCPOpCall = "call"
)

// Error codes in an MCPResult.
const (
	// MCPUnknownServer: the device config has no server of that name.
	MCPUnknownServer = "unknown_server"
	// MCPUnavailable: the server did not start, or is waiting before it
	// may start again after it failed.
	MCPUnavailable = "unavailable"
	// MCPBusy: the server already runs the most requests the device
	// allows.
	MCPBusy = "busy"
	// MCPTooLarge: the server's answer is larger than MaxMCPResult.
	MCPTooLarge  = "too_large"
	MCPTimeout   = "timeout"
	MCPCancelled = "cancelled"
	// MCPFailed: the server answered with an error, or exited.
	MCPFailed = "failed"
	// MCPRefused: the request was not valid, or the leg has no MCP.
	MCPRefused = "refused"
)

// MCP request limits.
const (
	// MCPDefaultTimeout applies to an MCP request without a timeout, and
	// MCPMaxTimeout caps every request.
	MCPDefaultTimeout = 120 * time.Second
	MCPMaxTimeout     = 3600 * time.Second
	// MaxMCPResult is the largest message the device reads from an MCP
	// server, and so the largest result of an MCPResult.
	MaxMCPResult = 16 << 20
	// MaxMCPRequest is the largest MCP request the device accepts.
	MaxMCPRequest = 4 << 20
)

// Error codes in a BrowserResult.
const (
	BrowserNotConnected = "not_connected"
	BrowserRefused      = "refused"
	BrowserTimeout      = "timeout"
	BrowserCancelled    = "cancelled"
	BrowserFailed       = "failed"
	// BrowserNotSignedIn: the browser is not signed in to the hub as the
	// device's owner. The hub or the device refused the request.
	BrowserNotSignedIn = "not_signed_in"
)

// Codes of a BrowserAuthResult that is not OK.
const (
	// BrowserAuthSignedOut answers an empty token.
	BrowserAuthSignedOut = "signed_out"
	// BrowserAuthInvalid: the hub does not know the token, or it was
	// revoked or expired.
	BrowserAuthInvalid = "invalid"
	// BrowserAuthWrongUser: the token belongs to another hub user than
	// the device's owner.
	BrowserAuthWrongUser = "wrong_user"
	// BrowserAuthOtherDevice: the token was issued for another device.
	BrowserAuthOtherDevice = "other_device"
	// BrowserAuthUnavailable: the hub could not check the token. The
	// device sends it again after a while.
	BrowserAuthUnavailable = "unavailable"
)

// BrowserTokenPrefix starts every browser credential the hub issues.
const BrowserTokenPrefix = "hbt_"

// BrowserExtensionID is the id of the Hermes Firefox extension. The hub
// accepts only its browser.identity redirect URL in the sign-in flow.
const BrowserExtensionID = "browser@hermes-device"

// BrowserRedirectURL is what Firefox's browser.identity.getRedirectURL()
// returns for the extension with id: the SHA-1 of the id in hex, under
// extensions.allizom.org. Firefox ends the sign-in flow when the hub
// redirects there, before any request leaves the browser.
func BrowserRedirectURL(id string) string {
	sum := sha1.Sum([]byte(id)) //nolint:gosec // Firefox derives the redirect host with SHA-1; nothing secret
	return "https://" + hex.EncodeToString(sum[:]) + ".extensions.allizom.org/"
}

// Browser request limits.
const (
	// BrowserDefaultTimeout applies to a Browser request without a
	// timeout, and BrowserMaxTimeout caps every request.
	BrowserDefaultTimeout = 60 * time.Second
	BrowserMaxTimeout     = 300 * time.Second
	// MaxBrowserResult is the largest BrowserResult the device sends. A
	// larger result is answered with BrowserFailed.
	MaxBrowserResult = 16 << 20
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
	// CloseServiceRestart: the gateway that holds the plugin is stopping.
	// Its commands cannot be resumed, so the device kills them at once.
	CloseServiceRestart = 1012
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
	// DefaultGrace is the plugin's default grace_seconds: how long both
	// sides keep the commands of a dropped version 3 leg for a resume.
	DefaultGrace = 120 * time.Second
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

// Hello is the first message from the device. Version is the oldest
// protocol version the device speaks and MaxVersion the newest (0 means
// Version), so a plugin that reads only Version still finds one it knows.
type Hello struct {
	Type          string   `json:"type"`
	Version       int      `json:"version"`
	MaxVersion    int      `json:"max_version,omitempty"`
	Roots         []string `json:"roots"`
	TempDir       string   `json:"tmp_dir"`
	Home          string   `json:"home"`
	Hostname      string   `json:"hostname"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Shell         string   `json:"shell"`
	ClientVersion string   `json:"client_version"`
	// Capabilities the device offers, such as CapabilityBrowser.
	Capabilities []string `json:"capabilities,omitempty"`
	// MCPServers names the MCP servers of the device config, with
	// CapabilityMCP.
	MCPServers []string `json:"mcp_servers,omitempty"`
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
	// GraceSeconds (version 3) is how long the plugin keeps the commands
	// of a dropped leg for a resume.
	GraceSeconds float64 `json:"grace_seconds,omitempty"`
	// Capabilities are the ones of hello's that the plugin accepts. A
	// plugin that does not know capabilities sends none.
	Capabilities []string `json:"capabilities,omitempty"`
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

// Output is a chunk of merged stdout and stderr. Offset (version 3) is the
// byte offset of Data's first byte in the command's output.
type Output struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
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

// Resume (version 3) lists the commands the plugin still waits for after a
// reconnect, each with the number of output bytes it already has.
type Resume struct {
	Type     string          `json:"type"`
	Commands []ResumeCommand `json:"commands"`
}

// ResumeCommand is one command of a Resume.
type ResumeCommand struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
}

// Browser asks the device to run Op with Args in the user's browser.
type Browser struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Args    json.RawMessage `json:"args,omitempty"`
	Timeout float64         `json:"timeout"`
}

// BrowserResult answers a Browser request. Code is set when OK is false.
type BrowserResult struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// BrowserStatus reports whether the browser extension is connected and
// signed in to the hub as the device's owner. The hub sets Connected to
// false while SignedIn is false, so a plugin that does not know SignedIn
// offers no browser either. Reason says why the browser cannot be used.
type BrowserStatus struct {
	Type             string `json:"type"`
	Connected        bool   `json:"connected"`
	SignedIn         bool   `json:"signed_in"`
	User             string `json:"user,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Browser          string `json:"browser,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
}

// BrowserAuth carries the extension's browser credential from the device
// to the hub. Token is empty when the browser is signed out.
type BrowserAuth struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// BrowserAuthResult tells the device whether the hub accepted its browser
// credential. User and SessionID are set when OK; Code and Error when not.
type BrowserAuthResult struct {
	Type      string `json:"type"`
	OK        bool   `json:"ok"`
	User      string `json:"user,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MCP asks the device for one operation on an MCP server (section 4.9).
// Server and Tool name the server and, for MCPOpCall, the tool;
// Arguments is the tool's arguments object.
type MCP struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	Server    string          `json:"server,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Timeout   float64         `json:"timeout"`
}

// MCPResult answers an MCP request. Code is set when OK is false.
type MCPResult struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
}
