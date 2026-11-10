// Package host is the native messaging host that Firefox starts for the
// Hermes extension. It dials the run daemon's browser socket and relays
// frames between it and Firefox, unchanged except for size limits.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/nativemsg"
)

// Name is the native messaging host name the extension connects to.
const Name = "hermes_device"

// ExtensionID is the id of the Hermes Firefox extension.
const ExtensionID = "browser@hermes-device"

// SocketFileName is the file next to the manifest that holds the daemon's
// socket path, so the host finds a socket outside the default state
// directory without help from Firefox's environment.
const SocketFileName = Name + ".socket"

const dialTimeout = 5 * time.Second

// ErrWrongExtension is returned when Firefox starts the host for another
// extension.
var ErrWrongExtension = errors.New("native host started for an unknown extension")

// Invocation reports whether args (os.Args) look like Firefox starting a
// native messaging host: <binary> <manifest path> <extension id>.
func Invocation(args []string) (manifest, extensionID string, ok bool) {
	if len(args) != 3 || !strings.HasSuffix(args[1], ".json") || !filepath.IsAbs(args[1]) {
		return "", "", false
	}
	return args[1], args[2], true
}

// Run relays between Firefox (in, out) and the daemon socket at sock until
// either side closes.
func Run(in io.Reader, out io.Writer, sock, extensionID string) error {
	if extensionID != ExtensionID {
		return fmt.Errorf("%w: %q", ErrWrongExtension, extensionID)
	}
	dctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dctx, "unix", sock)
	if err != nil {
		return fmt.Errorf("the hermes-device daemon is not running (dial %s): %w", sock, err)
	}
	defer c.Close()
	hello, err := json.Marshal(broker.Message{Type: broker.TypeHostHello, ExtensionID: extensionID, HostPID: os.Getpid()})
	if err != nil {
		return err
	}
	if err := nativemsg.Write(c, hello, nativemsg.MaxToBrowser); err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- pump(in, c, nativemsg.MaxFromBrowser, nativemsg.MaxFromBrowser) }()
	go func() { errc <- pump(c, out, nativemsg.MaxToBrowser, nativemsg.MaxToBrowser) }()
	err = <-errc
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func pump(r io.Reader, w io.Writer, readLimit, writeLimit int) error {
	for {
		msg, err := nativemsg.Read(r, readLimit)
		if err != nil {
			return err
		}
		if err := nativemsg.Write(w, msg, writeLimit); err != nil {
			return err
		}
	}
}

// Manifest is a Firefox native messaging host manifest.
type Manifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

// NewManifest returns the manifest for the binary at path.
func NewManifest(path string) Manifest {
	return Manifest{
		Name: Name, Description: "hermes-device browser control", Path: path, Type: "stdio",
		AllowedExtensions: []string{ExtensionID},
	}
}

// SocketFromManifest returns the socket path that install wrote next to
// the manifest at manifestPath, or "" when there is none.
func SocketFromManifest(manifestPath string) string {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), SocketFileName))
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(data))
	if !filepath.IsAbs(p) {
		return ""
	}
	return p
}

// ManifestDir is where Firefox looks for per-user native host manifests,
// under home.
func ManifestDir(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Mozilla", "NativeMessagingHosts")
	}
	return filepath.Join(home, ".mozilla", "native-messaging-hosts")
}
