package firefoxtest_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/bidi"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
)

const (
	// loginHost and twoLoginHost are in browser.autologin-hosts;
	// otherLoginHost is not. twoLoginHost has two saved logins.
	loginHost      = "127.0.0.4"
	otherLoginHost = "127.0.0.5"
	twoLoginHost   = "127.0.0.6"
	loginUser      = "alice"
	keyLogins      = "login_hosts"
	keySensitive   = "sensitive"
	keySelector    = "selector"
	keyAwait       = "awaitPromise"
	keyContext     = "context"
	keyExpr        = "expression"
	keyTarget      = "target"
)

type loginResult struct {
	TabID      int    `json:"tab_id"`
	Filled     bool   `json:"filled"`
	Submitted  bool   `json:"submitted"`
	LandedHost string `json:"landed_host"`
	Reason     string `json:"reason"`
}

// loginServer records the posts it gets.
type loginServer struct {
	mu    sync.Mutex
	posts []string
}

func (l *loginServer) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.posts...)
}

// ServeHTTP serves a login form on /, one that posts to a 307 redirect
// to otherLoginHost on /bounce-form, one that uses GET on /get-form, and
// /login, which takes the post and redirects to /home.
func (l *loginServer) handler(other string) http.HandlerFunc {
	form := func(action, method string) string {
		return `<!doctype html><title>Sign in</title><h1>Sign in</h1><form method="` + method + `" action="` + action + `">` +
			`<label>Email <input name="user" autocomplete="username"></label>` +
			`<label>Password <input type="password" name="pass" autocomplete="current-password"></label>` +
			`<button type="button" onclick="this.closest('form').pass.type='text'">Show</button>` +
			`<button type="submit">Sign in</button></form>`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, form("/login", "post"))
		case "/bounce-form":
			_, _ = io.WriteString(w, form("/bounce", "post"))
		case "/get-form":
			_, _ = io.WriteString(w, form("/login", "get"))
		case "/confirm":
			_, _ = io.WriteString(w, `<!doctype html><title>Confirm</title><form method="post" action="/login">`+
				`<input type="password" name="pass" autocomplete="current-password"><button>Confirm</button></form>`)
		case "/prefilled":
			_, _ = io.WriteString(w, `<!doctype html><title>Prefilled</title><form method="post" action="/login">`+
				`<input name="user"><input type="password" name="pass"></form>`+
				`<script>document.querySelector("[name=pass]").value = "typed-by-page"</script>`)
		case "/bounce":
			http.Redirect(w, r, other+"/login", http.StatusTemporaryRedirect)
		case "/login":
			body, _ := io.ReadAll(r.Body)
			l.mu.Lock()
			l.posts = append(l.posts, r.Method+" "+string(body))
			l.mu.Unlock()
			http.Redirect(w, r, "/home", http.StatusSeeOther)
		case "/home":
			_, _ = io.WriteString(w, `<!doctype html><title>Home</title><p>welcome</p>`)
		default:
			http.NotFound(w, r)
		}
	}
}

// testCA returns a CA certificate (DER) and a server certificate it signed
// for the hosts.
func testCA(t *testing.T, hosts ...string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hermes-device test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: hosts[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range hosts {
		leaf.IPAddresses = append(leaf.IPAddresses, net.ParseIP(h))
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return caDER, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func serveTLSOn(ctx context.Context, t *testing.T, host string, cert tls.Certificate, h http.Handler) *httptest.Server {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", host+":0")
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// chromeEval runs a script in Firefox's privileged browser window, which
// --remote-allow-system-access opens to BiDi. Only the test uses it, to
// trust the test CA and to save logins the way the user would.
func chromeEval(ctx context.Context, t *testing.T, bc *bidi.Client, expr string) string {
	t.Helper()
	res, err := bc.Call(ctx, "browsingContext.getTree", map[string]any{"moz:scope": "chrome"})
	if err != nil {
		t.Fatalf("chrome contexts: %v", err)
	}
	var tree struct {
		Contexts []struct {
			Context string `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(res, &tree); err != nil || len(tree.Contexts) == 0 {
		t.Fatalf("no chrome context: %v %s", err, res)
	}
	out, err := bc.Call(ctx, "script.evaluate", map[string]any{keyExpr: expr, keyAwait: true,
		keyTarget: map[string]any{keyContext: tree.Contexts[0].Context}})
	if err != nil {
		t.Fatalf("chrome script: %v", err)
	}
	var r struct {
		Type   string `json:"type"`
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Type != "success" {
		t.Fatalf("chrome script failed: %s", out)
	}
	return fmt.Sprint(r.Result.Value)
}

// seedLogins trusts the CA and saves logins in the test profile's own
// login storage (Services.logins), as Firefox does when the user saves one.
func seedLogins(ctx context.Context, t *testing.T, bc *bidi.Client, caDER []byte, logins [][3]string) {
	t.Helper()
	js := `(async () => {
  const db = Cc["@mozilla.org/security/x509certdb;1"].getService(Ci.nsIX509CertDB);
  db.addCertFromBase64(` + jsJSON(t, base64.StdEncoding.EncodeToString(caDER)) + `, "C,,");
  for (const [origin, user, pass] of ` + jsJSON(t, logins) + `) {
    const li = Cc["@mozilla.org/login-manager/loginInfo;1"].createInstance(Ci.nsILoginInfo);
    li.init(origin, origin, null, user, pass, "user", "pass");
    await Services.logins.addLoginAsync(li);
  }
  return (await Services.logins.getAllLogins()).length;
})()`
	if n := chromeEval(ctx, t, bc, js); n != fmt.Sprint(len(logins)) {
		t.Fatalf("saved logins = %s, want %d", n, len(logins))
	}
}

func jsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recorded calls the broker and keeps every answer and error, so the test
// can check that the password never comes back.
type recorded struct {
	b   *broker.Broker
	out []string
}

func (r *recorded) call(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := r.b.Call(cctx, op, args)
	r.out = append(r.out, string(res))
	if err != nil {
		r.out = append(r.out, err.Error())
	}
	return res, err
}

func (r *recorded) must(ctx context.Context, t *testing.T, op string, args map[string]any, out any) {
	t.Helper()
	res, err := r.call(ctx, op, args)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			t.Fatalf("%s: %v: %s", op, err, res)
		}
	}
}

func (r *recorded) refused(ctx context.Context, t *testing.T, what, want, op string, args map[string]any) {
	t.Helper()
	_, err := r.call(ctx, op, args)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: err = %v, want %q", what, err, want)
	}
}

// loginFlow checks browser_login (docs/browser.md section 7.4) with real
// saved logins in the test profile: Firefox itself fills the form.
func loginFlow(ctx context.Context, t *testing.T, b *broker.Broker, bc *bidi.Client, firstTab int) {
	t.Helper()
	password := "pw-" + rand.Text()
	caDER, cert := testCA(t, loginHost, otherLoginHost, twoLoginHost)
	other := &loginServer{}
	otherSrv := serveTLSOn(ctx, t, otherLoginHost, cert, other.handler(""))
	main := &loginServer{}
	mainSrv := serveTLSOn(ctx, t, loginHost, cert, main.handler(otherSrv.URL))
	two := &loginServer{}
	twoSrv := serveTLSOn(ctx, t, twoLoginHost, cert, two.handler(""))
	seedLogins(ctx, t, bc, caDER, [][3]string{
		{mainSrv.URL, loginUser, password},
		{otherSrv.URL, loginUser, password},
		{twoSrv.URL, loginUser, password},
		{twoSrv.URL, "bob", password + "-b"},
	})
	r := &recorded{b: b}
	rig := &loginRig{r: r, main: main, other: other, two: two, mainURL: mainSrv.URL, twoURL: twoSrv.URL,
		otherURL: otherSrv.URL}
	open := func(u string) tabResult { return rig.open(ctx, t, u) }

	// Not in browser.autologin-hosts: refused, even with an approval.
	otherTab := open(otherSrv.URL + "/")
	r.refused(ctx, t, "login on a host not in the list", "browser.autologin-hosts", "login",
		map[string]any{keyTab: otherTab.TabID, keyGuard: approve(otherLoginHost)})
	if got := other.got(); len(got) != 0 {
		t.Errorf("the unlisted host got posts: %d", len(got))
	}

	// The tab opens in the background; Firefox fills it once browser_login
	// shows it.
	tab := open(mainSrv.URL + "/")
	var before struct {
		Tabs []struct {
			TabID  int  `json:"tab_id"`
			Active bool `json:"active"`
		} `json:"tabs"`
	}
	r.must(ctx, t, "tabs_list", map[string]any{}, &before)
	activeBefore := -1
	for _, x := range before.Tabs {
		if x.Active {
			activeBefore = x.TabID
		}
		if x.TabID == tab.TabID && x.Active {
			t.Errorf("the login tab opened in the foreground")
		}
	}
	t.Logf("active agent tab before login: %d (first tab %d)", activeBefore, firstTab)
	loginRefusals(ctx, t, r, tab.TabID)

	var check map[string]any
	r.must(ctx, t, "check", map[string]any{keyTab: tab.TabID}, &check)
	t.Logf("check: %v", check)
	if a, ok := check["after_activate"].(map[string]any); !ok || a["visibility"] != "visible" || check["restored"] != true {
		t.Errorf("check = %v", check)
	}

	res := rig.login(ctx, t, map[string]any{keyTab: tab.TabID, keyGuard: approve(loginHost)})
	t.Logf("login: %+v", res)
	if !res.Filled || !res.Submitted || res.LandedHost != loginHost || res.Reason != "" {
		t.Errorf("login = %+v", res)
	}
	want := "POST user=" + url.QueryEscape(loginUser) + "&pass=" + url.QueryEscape(password)
	if got := main.got(); len(got) != 1 || got[0] != want {
		t.Errorf("login server got %q, want %q", got, want)
	}
	var text textResult
	r.must(ctx, t, "read_text", map[string]any{keyTab: tab.TabID}, &text)
	if text.Text != "welcome" {
		t.Errorf("after login: %s %q", text.URL, text.Text)
	}
	var after struct {
		Tabs []struct {
			TabID  int  `json:"tab_id"`
			Active bool `json:"active"`
		} `json:"tabs"`
	}
	r.must(ctx, t, "tabs_list", map[string]any{}, &after)
	for _, x := range after.Tabs {
		if x.TabID == tab.TabID && x.Active && activeBefore != tab.TabID {
			t.Errorf("the previously active tab was not restored")
		}
	}

	loginEdges(ctx, t, rig, tab.TabID)

	for _, out := range r.out {
		if strings.Contains(out, password) {
			t.Errorf("a browser answer holds the password: %.200s", out)
		}
	}
	t.Logf("checked %d answers for the password", len(r.out))
}

// loginRig holds the login test servers and the recorded calls.
type loginRig struct {
	r                *recorded
	main, other, two *loginServer
	mainURL, twoURL  string
	otherURL         string
}

func (g *loginRig) open(ctx context.Context, t *testing.T, u string) tabResult {
	t.Helper()
	var tab tabResult
	host := strings.Split(strings.TrimPrefix(u, "https://"), ":")[0]
	g.r.must(ctx, t, "tab_open", map[string]any{keyURL: u, keyGuard: map[string]any{keyVisit: []string{host}}}, &tab)
	return tab
}

func (g *loginRig) login(ctx context.Context, t *testing.T, args map[string]any) loginResult {
	t.Helper()
	var res loginResult
	g.r.must(ctx, t, "login", args, &res)
	return res
}

func approve(host string) map[string]any { return map[string]any{keyLogins: []string{host}} }

// loginEdges checks a login from a URL, two saved logins, a GET form and
// a 307 redirect to another host.
func loginEdges(ctx context.Context, t *testing.T, rig *loginRig, tabID int) {
	t.Helper()
	// From a URL, in a new tab.
	res := rig.login(ctx, t, map[string]any{keyURL: rig.mainURL + "/", keyGuard: approve(loginHost)})
	if !res.Filled || !res.Submitted || res.LandedHost != loginHost || res.TabID == tabID {
		t.Errorf("login from a URL = %+v", res)
	}

	// Two saved logins: Firefox fills nothing, and the reason says why.
	twoTab := rig.open(ctx, t, rig.twoURL+"/")
	res = rig.login(ctx, t, map[string]any{keyTab: twoTab.TabID, keyGuard: approve(twoLoginHost), "timeout_ms": 3000})
	t.Logf("login with two saved logins: %+v", res)
	if res.Filled || res.Submitted || !strings.Contains(res.Reason, "more than one login") {
		t.Errorf("login with two saved logins = %+v", res)
	}
	if got := rig.two.got(); len(got) != 0 {
		t.Errorf("unfilled login posted: %q", got)
	}

	// A GET form would put the password into the address.
	rig.r.must(ctx, t, "navigate", map[string]any{keyTab: tabID, keyURL: rig.mainURL + "/get-form"}, nil)
	res = rig.login(ctx, t, map[string]any{keyTab: tabID, keyGuard: approve(loginHost)})
	if !res.Filled || res.Submitted || !strings.Contains(res.Reason, "POST") {
		t.Errorf("login on a GET form = %+v", res)
	}

	// A 307 redirect would post the form again to another host: cancelled.
	var nav tabResult
	rig.r.must(ctx, t, "navigate", map[string]any{keyTab: tabID, keyURL: rig.mainURL + "/bounce-form"}, &nav)
	t.Logf("navigated to %s", nav.URL)
	res = rig.login(ctx, t, map[string]any{keyTab: tabID, keyGuard: approve(loginHost)})
	t.Logf("login through a 307 to another host: %+v", res)
	if !res.Filled || !res.Submitted || res.Reason != "" {
		t.Errorf("login through a 307 = %+v", res)
	}
	if got := rig.other.got(); len(got) != 0 {
		t.Errorf("the 307 target got the login post: %q", got)
	}

	// A form with only a password field (a confirmation) is not submitted.
	rig.r.must(ctx, t, "navigate", map[string]any{keyTab: tabID, keyURL: rig.mainURL + "/confirm"}, nil)
	res = rig.login(ctx, t, map[string]any{keyTab: tabID, keyGuard: approve(loginHost)})
	if res.Filled || res.Submitted || !strings.Contains(res.Reason, "no username field") {
		t.Errorf("login on a password-only form = %+v", res)
	}

	// Off the list: no script reads a filled password, and check does not
	// show a tab with a password field.
	other := rig.open(ctx, t, rig.otherURL+"/prefilled")
	rig.r.refused(ctx, t, "eval on a page with a filled password", "filled password", "eval", map[string]any{keyTab: other.TabID,
		"script": "return 1", keyGuard: map[string]any{keyScripts: []string{otherLoginHost}, keySensitive: true}})
	var check map[string]any
	rig.r.must(ctx, t, "check", map[string]any{keyTab: other.TabID}, &check)
	if _, shown := check["after_activate"]; shown || !strings.Contains(fmt.Sprint(check["note"]), "not activated") {
		t.Errorf("check on an unlisted password page = %v", check)
	}
}

// loginRefusals checks what is refused on a login page of an autologin
// host, and that its snapshot shows no values.
func loginRefusals(ctx context.Context, t *testing.T, r *recorded, tabID int) {
	t.Helper()
	var snap snapResult
	r.must(ctx, t, "snapshot", map[string]any{keyTab: tabID}, &snap)
	t.Logf("login page snapshot:\n%s", snap.Snapshot)
	if strings.Contains(snap.Snapshot, "value=") {
		t.Errorf("snapshot on an autologin host shows a value: %s", snap.Snapshot)
	}
	r.refused(ctx, t, "eval on an autologin host", "browser.autologin-hosts", "eval", map[string]any{keyTab: tabID,
		"script": "return document.querySelector('[name=pass]').value", keyGuard: map[string]any{keyScripts: []string{loginHost}, keySensitive: true}})
	r.refused(ctx, t, "type into the password field", "browser_login", "type", map[string]any{keyTab: tabID,
		keyRef: refByName(snap.Snapshot, "Password"), keyText: "x", keyGuard: map[string]any{keySensitive: true}})
	r.refused(ctx, t, "click the show button", "browser_login", "click", map[string]any{keyTab: tabID,
		keyRef: refByName(snap.Snapshot, "Show"), keyGuard: map[string]any{keySensitive: true}})
	r.refused(ctx, t, "screenshot of the login page", "autologin", "screenshot", map[string]any{keyTab: tabID})
	r.refused(ctx, t, "selector wait on the login page", "autologin", "wait",
		map[string]any{keyTab: tabID, keySelector: "input[name=pass]:placeholder-shown", "timeout_ms": 500})
	r.refused(ctx, t, "login without approval", "needs the user's approval", "login", map[string]any{keyTab: tabID})
}
