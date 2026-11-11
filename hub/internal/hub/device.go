package hub

import (
	"context"
	"errors"
	"time"
)

// Device is a paired hermes-device client: one computer of one user,
// pinned to one backend at pairing.
type Device struct {
	ID            string
	User          string
	BackendID     string
	Name          string
	Hostname      string
	OS            string
	Arch          string
	ClientVersion string
	CreatedAt     time.Time
	// Paired is false until the device fetched its token.
	Paired     bool
	LastSeenAt time.Time
	LastAddr   string
	// PrevAddr and AddrChangedAt describe the latest change of address
	// the user has not dismissed. AddrChangedAt is zero when there is none.
	PrevAddr      string
	AddrChangedAt time.Time
	// Service is a device from the hub config (backends[].service_devices),
	// such as a sandbox pod. It may use only the exec leg.
	Service bool
	// LastClose is how the device's latest exec leg ended, when no exec leg
	// opened since. Its At is zero otherwise.
	LastClose LegClose
}

// LegClose is how a device leg ended: the WebSocket close code and reason
// the device got, the side that ended it (CloseBy*), and when.
type LegClose struct {
	Code   int
	Reason string
	By     string
	At     time.Time
}

// Sides that end a device leg.
const (
	CloseByDevice  = "device"
	CloseByBackend = "backend"
	CloseByHub     = "hub"
)

// ServiceUser is the assertion sub of service devices, which belong to a
// backend, not a user.
const ServiceUser = "service"

// Sandbox is a service device as a user's device list shows it. Online is
// nil when the backend could not be asked.
type Sandbox struct {
	ID, Name, BackendID string
	Online              *bool
}

// PairingRequest is what a device sends to start pairing, and the address
// it came from.
type PairingRequest struct {
	Name          string
	Hostname      string
	OS            string
	Arch          string
	ClientVersion string
	Addr          string
}

// Pairing is a pending pairing request, found by its user code.
type Pairing struct {
	UserCode string
	PairingRequest
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Pairing poll outcomes, the error codes of RFC 8628 section 3.5, and
// PairApproved.
const (
	PairPending  = "authorization_pending"
	PairSlowDown = "slow_down"
	PairExpired  = "expired_token"
	PairDenied   = "access_denied"
	PairApproved = "approved"
)

// PairPoll is the outcome of one poll. Token and Device are set only for
// PairApproved, which a pairing answers once.
type PairPoll struct {
	Status string
	Token  string
	Device Device
}

// Device store errors.
var (
	ErrNameTaken      = errors.New("device name in use")
	ErrUnknownPairing = errors.New("unknown or expired pairing code")
	ErrUnknownDevice  = errors.New("unknown device")
)

// DeviceStore keeps devices and pairing requests. Only hashes of device
// tokens and device codes are stored.
type DeviceStore interface {
	BrowserStore
	// StartPairing records a pairing request valid for ttl and returns its
	// secret device code and the user code the user types.
	StartPairing(ctx context.Context, req PairingRequest, ttl time.Duration) (deviceCode, userCode string, err error)
	// PendingPairing finds a pairing waiting for confirmation by its user
	// code, in any case and with or without its dash.
	PendingPairing(ctx context.Context, userCode string) (Pairing, bool, error)
	// ApprovePairing creates the device for user and backendID under name.
	// It fails with ErrUnknownPairing or ErrNameTaken.
	ApprovePairing(ctx context.Context, userCode, user, backendID, name string) (Device, error)
	// DenyPairing declines a pending pairing.
	DenyPairing(ctx context.Context, userCode string) error
	// PollPairing answers a device's poll. A poll sooner than interval
	// after the last one answers PairSlowDown.
	PollPairing(ctx context.Context, deviceCode string, interval time.Duration) (PairPoll, error)
	// DeviceByToken returns the paired, unrevoked device of a token.
	DeviceByToken(ctx context.Context, token string) (Device, bool, error)
	// Device returns an unrevoked device by id.
	Device(ctx context.Context, id string) (Device, bool, error)
	// Devices lists a user's unrevoked devices, oldest first.
	Devices(ctx context.Context, user string) ([]Device, error)
	// RevokeDevice revokes one of user's devices. It reports whether one
	// was revoked.
	RevokeDevice(ctx context.Context, user, id string) (bool, error)
	// SeenDevice records a request from the device at addr. A new address
	// is remembered as a change the user sees until DismissAddressChange.
	SeenDevice(ctx context.Context, id, addr string) error
	// DismissAddressChange clears the address change note of user's device.
	DismissAddressChange(ctx context.Context, user, id string) error
	// ExecLegOpened records that the exec leg legID of the device opened,
	// and clears its LastClose. legID is unique to the leg.
	ExecLegOpened(ctx context.Context, id, legID string) error
	// ExecLegClosed records how the exec leg legID ended, unless another
	// exec leg of the device opened since.
	ExecLegClosed(ctx context.Context, id, legID string, c LegClose) error
	// CodeFailure records a wrong pairing code typed by user and returns
	// how many they typed within window.
	CodeFailure(ctx context.Context, user string, window time.Duration) (int, error)
	// CodeFailures counts the wrong pairing codes user typed within window.
	CodeFailures(ctx context.Context, user string, window time.Duration) (int, error)
	// UseDeviceDir records that a session started or moved to dir on a
	// device. A device keeps its RecentDirs latest directories.
	UseDeviceDir(ctx context.Context, deviceID, dir string) error
	// DeviceDirs lists a device's recent directories, latest first.
	DeviceDirs(ctx context.Context, deviceID string) ([]string, error)
}

// RecentDirs is how many recent directories the hub keeps per device.
const RecentDirs = 8

// Computer is one of a user's paired computers, as the chat pages offer it
// for a chat to run on. Home and Roots are known only while it is
// online.
type Computer struct {
	ID, Name, BackendID string
	Online              bool
	Hostname, OS        string
	Home                string
	Roots               []string
	// Recent are the latest directories chats started or moved to on it.
	Recent []string
}

// Folder is one directory of a computer and its subdirectories.
type Folder struct {
	Path string
	Dirs []string
}

// Where a session runs its commands and file changes.
const (
	// PlaceComputer is a session bound to a computer.
	PlaceComputer = "computer"
	// PlaceSandbox is a session that was never bound, which the backend
	// runs on its sandbox device.
	PlaceSandbox = "sandbox"
	// PlaceServer is a session that was never bound, which the backend
	// runs on its own server (the plugin's fallback is not the sandbox).
	PlaceServer = "server"
	// PlaceNone is a session that was bound once and is no longer: it
	// runs no commands until it is bound again.
	PlaceNone = "none"
)

// Place says where a session runs its commands and file changes.
type Place struct {
	Kind string `json:"kind"`
	// DeviceID, Name and Dir name the computer or sandbox and the
	// directory on it.
	DeviceID string `json:"device_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Dir      string `json:"dir,omitempty"`
	Online   bool   `json:"online"`
	// Mine is true for one of the user's own computers.
	Mine bool `json:"mine,omitempty"`
	// Inherited is a binding of an earlier part of the chat (a session
	// it was compressed or branched from), which the chat follows.
	Inherited bool `json:"inherited,omitempty"`
}

// SessionRef names one session on one backend.
type SessionRef struct {
	BackendID string `json:"backend_id"`
	SessionID string `json:"session_id"`
}

// Computers lets users run chats on their paired computers. Every method
// checks that user may use the backend and owns the computer.
type Computers interface {
	// Computers lists user's paired computers, not sandboxes.
	Computers(ctx context.Context, user string) ([]Computer, error)
	// Folders lists the subdirectories of path on one of user's
	// computers; "" is the computer's first root.
	Folders(ctx context.Context, user, deviceID, path string) (Folder, error)
	// Places says where each session runs. A session it could not find
	// out about is left out.
	Places(ctx context.Context, user string, sessions []SessionRef) map[SessionRef]Place
	// Place says where one session runs. Kind is "" on a backend without
	// devices.
	Place(ctx context.Context, user, backendID, sessionID string) (Place, error)
	// StartOn starts a session on one of user's computers in dir, bound
	// before any prompt can reach it. "" is the computer's first root.
	StartOn(ctx context.Context, user, deviceID, dir string) (Session, Place, error)
	// MoveTo binds a session to one of user's computers in dir, also when
	// it ran on the sandbox or another of user's computers.
	MoveTo(ctx context.Context, user, backendID, sessionID, deviceID, dir string) (Place, error)
	// Release unbinds a session from user's computer. The session then
	// runs no commands, also not on the sandbox, until it is moved again.
	Release(ctx context.Context, user, backendID, sessionID string) error
}

// Computer errors. ErrFolderRefused and ErrCannotMove wrap a message for
// the user.
var (
	ErrComputerOffline = errors.New("computer offline")
	ErrFolderRefused   = errors.New("folder refused")
	ErrCannotMove      = errors.New("session cannot run on a computer")
	ErrOtherBackend    = errors.New("computer works with another backend")
	ErrNotOnComputer   = errors.New("session is not on a computer")
	ErrInheritedPlace  = errors.New("session follows the computer of the session it came from")
)
