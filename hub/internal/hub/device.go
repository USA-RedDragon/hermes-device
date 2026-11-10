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
}

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
