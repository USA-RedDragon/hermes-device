package protocol_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

// shared is one constant that Go and the Python plugin both define. Names
// lists the Python names it may have, preferred first. Durations are
// seconds on the Python side.
type shared struct {
	goName string
	value  any
	names  []string
}

func protocolConstants() []shared {
	return []shared{
		{"Version", protocol.Version, []string{"PROTOCOL_VERSION", "VERSION"}},
		{"MinVersion", protocol.MinVersion, []string{"MIN_PROTOCOL_VERSION"}},
		{"ResumeVersion", protocol.ResumeVersion, []string{"RESUME_VERSION"}},
		{"CloseServiceRestart", protocol.CloseServiceRestart, []string{"CLOSE_SERVICE_RESTART"}},
		{"DefaultGrace", protocol.DefaultGrace, []string{"DEFAULT_GRACE_S"}},
		{"TypeHello", protocol.TypeHello, []string{"TYPE_HELLO"}},
		{"TypeWelcome", protocol.TypeWelcome, []string{"TYPE_WELCOME"}},
		{"TypeExec", protocol.TypeExec, []string{"TYPE_EXEC"}},
		{"TypeKill", protocol.TypeKill, []string{"TYPE_KILL"}},
		{"TypeOutput", protocol.TypeOutput, []string{"TYPE_OUTPUT"}},
		{"TypeExit", protocol.TypeExit, []string{"TYPE_EXIT"}},
		{"TypePing", protocol.TypePing, []string{"TYPE_PING"}},
		{"TypePong", protocol.TypePong, []string{"TYPE_PONG"}},
		{"TypeResume", protocol.TypeResume, []string{"TYPE_RESUME"}},
		{"CloseUnauthorized", protocol.CloseUnauthorized, []string{"CLOSE_UNAUTHORIZED"}},
		{"CloseAssertionRefused", protocol.CloseAssertionRefused, []string{"CLOSE_ASSERTION_REFUSED"}},
		{"CloseReplaced", protocol.CloseReplaced, []string{"CLOSE_REPLACED"}},
		{"CloseUnsupportedVersion", protocol.CloseUnsupportedVersion, []string{"CLOSE_UNSUPPORTED_VERSION"}},
		{"CloseUnsupportedPlatform", protocol.CloseUnsupportedPlatform, []string{"CLOSE_UNSUPPORTED_PLATFORM"}},
		{"CloseBackendUnavailable", protocol.CloseBackendUnavailable, []string{"CLOSE_BACKEND_UNAVAILABLE"}},
		{"ClosePingTimeout", protocol.ClosePingTimeout, []string{"CLOSE_PING_TIMEOUT"}},
		{"ExitRefused", protocol.ExitRefused, []string{"EXIT_REFUSED"}},
		{"ExitKilled", protocol.ExitKilled, []string{"EXIT_KILLED"}},
		{"ExitLost", protocol.ExitLost, []string{"EXIT_LOST"}},
		{"PingInterval", protocol.PingInterval, []string{"PING_INTERVAL_S", "PING_INTERVAL"}},
		{"PingTimeout", protocol.PingTimeout, []string{"PING_TIMEOUT_S", "PING_TIMEOUT"}},
		{"HelloTimeout", protocol.HelloTimeout, []string{"HELLO_TIMEOUT_S", "HELLO_TIMEOUT"}},
		{"PluginExecPath", protocol.PluginExecPath, []string{"PLUGIN_EXEC_PATH", "EXEC_PATH"}},
		{"TypeBrowser", protocol.TypeBrowser, []string{"TYPE_BROWSER"}},
		{"TypeBrowserResult", protocol.TypeBrowserResult, []string{"TYPE_BROWSER_RESULT"}},
		{"TypeBrowserStatus", protocol.TypeBrowserStatus, []string{"TYPE_BROWSER_STATUS"}},
		{"CapabilityBrowser", protocol.CapabilityBrowser, []string{"CAPABILITY_BROWSER"}},
		{"BrowserNotConnected", protocol.BrowserNotConnected, []string{"BROWSER_NOT_CONNECTED"}},
		{"BrowserRefused", protocol.BrowserRefused, []string{"BROWSER_REFUSED"}},
		{"BrowserTimeout", protocol.BrowserTimeout, []string{"BROWSER_TIMEOUT"}},
		{"BrowserCancelled", protocol.BrowserCancelled, []string{"BROWSER_CANCELLED"}},
		{"BrowserFailed", protocol.BrowserFailed, []string{"BROWSER_FAILED"}},
		{"BrowserNotSignedIn", protocol.BrowserNotSignedIn, []string{"BROWSER_NOT_SIGNED_IN"}},
		{"TypeBrowserAuth", protocol.TypeBrowserAuth, []string{"TYPE_BROWSER_AUTH"}},
		{"TypeBrowserAuthResult", protocol.TypeBrowserAuthResult, []string{"TYPE_BROWSER_AUTH_RESULT"}},
		{"BrowserDefaultTimeout", protocol.BrowserDefaultTimeout, []string{"BROWSER_DEFAULT_TIMEOUT_S"}},
		{"BrowserMaxTimeout", protocol.BrowserMaxTimeout, []string{"BROWSER_MAX_TIMEOUT_S"}},
		{"MaxBrowserResult", protocol.MaxBrowserResult, []string{"MAX_BROWSER_RESULT"}},
	}
}

func assertionConstants() []shared {
	return []shared{
		{"Header", assertion.Header, []string{"HEADER", "ASSERTION_HEADER"}},
		{"Prefix", assertion.Prefix, []string{"PREFIX", "ASSERTION_PREFIX"}},
		{"Type", assertion.Type, []string{"TYPE", "ASSERTION_TYPE", "TYP"}},
		{"MaxLifetime", assertion.MaxLifetime, []string{"MAX_LIFETIME_S", "MAX_LIFETIME"}},
		{"ClockSkew", assertion.ClockSkew, []string{"CLOCK_SKEW_S", "CLOCK_SKEW"}},
		{"MaxEncodedLen", assertion.MaxEncodedLen, []string{"MAX_ENCODED_LEN"}},
		{"LegExec", assertion.LegExec, []string{"LEG_EXEC"}},
		{"LegACP", assertion.LegACP, []string{"LEG_ACP"}},
	}
}

func TestPythonCoreConstants(t *testing.T) {
	t.Parallel()
	checkPython(t, filepath.Join("..", "plugin", "device", "core.py"), protocolConstants())
}

func TestPythonAssertionConstants(t *testing.T) {
	t.Parallel()
	checkPython(t, filepath.Join("..", "plugin", "device", "assertion.py"), assertionConstants())
}

func checkPython(t *testing.T, path string, consts []shared) {
	t.Helper()
	values, err := pythonConstants(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s is absent", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range consts {
		t.Run(c.goName, func(t *testing.T) {
			t.Parallel()
			for _, name := range c.names {
				raw, ok := values[name]
				if !ok {
					continue
				}
				got, err := parseLiteral(raw)
				if err != nil {
					t.Fatalf("%s %s = %s: %v", path, name, raw, err)
				}
				if want := pythonValue(c.value); !equal(got, want) {
					t.Errorf("%s %s = %s, Go %s = %v", path, name, raw, c.goName, want)
				}
				return
			}
			t.Skipf("%s defines none of %v", path, c.names)
		})
	}
}

var assignment = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)\s*(?::\s*[^=]+)?=\s*(.+)$`)

// pythonConstants returns the right-hand side of every top-level
// UPPER_CASE assignment in path.
func pythonConstants(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := assignment.FindStringSubmatch(sc.Text()); m != nil {
			if _, dup := out[m[1]]; !dup {
				out[m[1]] = stripComment(strings.TrimSpace(m[2]))
			}
		}
	}
	return out, sc.Err()
}

func stripComment(s string) string {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0 && c == '\\':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && c == '#':
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

// parseLiteral reads a Python int, float or plain string literal.
func parseLiteral(s string) (any, error) {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		body := s[1 : len(s)-1]
		if strings.ContainsAny(body, "\\"+s[:1]) {
			return nil, fmt.Errorf("string with escapes or quotes is not supported")
		}
		return body, nil
	}
	n := strings.ReplaceAll(s, "_", "")
	if i, err := strconv.ParseInt(n, 10, 64); err == nil {
		return float64(i), nil
	}
	if f, err := strconv.ParseFloat(n, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("not an int, float or string literal")
}

func pythonValue(v any) any {
	switch v := v.(type) {
	case time.Duration:
		return v.Seconds()
	case int:
		return float64(v)
	default:
		return v
	}
}

func equal(a, b any) bool { return a == b }
