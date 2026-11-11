package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Registry errors.
var (
	ErrAuth     = errors.New("registry wants credentials; only anonymous pulls are supported")
	ErrNotFound = errors.New("not found in the registry")
	ErrDigest   = errors.New("content does not match its digest")
	ErrTooLarge = errors.New("content is larger than allowed")
)

const maxTokenBody = 1 << 20

// DefaultIdleTimeout is how long a registry request may go without data:
// for the answer to start, and between reads of its body. Large layers on
// a slow link take as long as they need while data flows.
const DefaultIdleTimeout = 2 * time.Minute

// ErrIdle is a registry request that stalled for its idle timeout.
var ErrIdle = errors.New("the registry stopped sending data")

// idleBody cancels its request when no data arrives for IdleTimeout.
type idleBody struct {
	io.ReadCloser
	ctx    context.Context //nolint:containedctx // the request's own context, to report its cause
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelCauseFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && errors.Is(context.Cause(b.ctx), ErrIdle) {
		err = ErrIdle
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err
}

// registry talks to one repository of one registry with anonymous
// bearer tokens.
type registry struct {
	http *http.Client
	ref  Reference
	ua   string
	idle time.Duration

	mu    sync.Mutex
	token string
}

// get sends GET path with accept and, on a bearer challenge, fetches an
// anonymous token and sends it again. The caller closes the body of a
// 200 answer; other answers are errors.
func (r *registry) get(ctx context.Context, path, accept string) (*http.Response, error) {
	u := "https://" + r.ref.host() + "/v2/" + r.ref.Repository + path
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithCancelCause(ctx)
		timer := time.AfterFunc(r.idle, func() { cancel(ErrIdle) })
		stop := func() {
			timer.Stop()
			cancel(nil)
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
		if err != nil {
			stop()
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if r.ua != "" {
			req.Header.Set("User-Agent", r.ua)
		}
		r.mu.Lock()
		tok := r.token
		r.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			if errors.Is(context.Cause(rctx), ErrIdle) {
				err = fmt.Errorf("%s: %w", r.ref.Domain, ErrIdle)
			}
			stop()
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body = &idleBody{ReadCloser: resp.Body, ctx: rctx, timer: timer, idle: r.idle, cancel: cancel}
			return resp, nil
		}
		drain(resp)
		stop()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			challenge := resp.Header.Get("WWW-Authenticate")
			if err := r.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("%s%s: %w", r.ref.Domain+"/"+r.ref.Repository, path, ErrNotFound)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("%s: %w (%s)", r.ref.Domain, ErrAuth, resp.Status)
		default:
			return nil, fmt.Errorf("%s%s: registry answered %s", r.ref.Domain+"/"+r.ref.Repository, path, resp.Status)
		}
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// authenticate fetches an anonymous pull token for the bearer challenge.
func (r *registry) authenticate(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("%s: %w (challenge %q)", r.ref.Domain, ErrAuth, scheme)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return fmt.Errorf("%s: bad token realm %q", r.ref.Domain, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+r.ref.Repository+":pull")
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	if r.ua != "" {
		req.Header.Set("User-Agent", r.ua)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s token: %w", r.ref.Domain, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s token: %w (%s)", r.ref.Domain, ErrAuth, resp.Status)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBody)).Decode(&out); err != nil {
		return fmt.Errorf("%s token: %w", r.ref.Domain, err)
	}
	tok := out.Token
	if tok == "" {
		tok = out.AccessToken
	}
	if tok == "" {
		return fmt.Errorf("%s token: empty answer", r.ref.Domain)
	}
	r.mu.Lock()
	r.token = tok
	r.mu.Unlock()
	return nil
}

// parseChallenge reads `Scheme k="v",k2=v2` (RFC 7235). Quoted values
// may hold commas, as a scope with several actions does.
func parseChallenge(h string) (scheme string, params map[string]string) {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	params = map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimLeft(rest[eq+1:], " ")
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				b.WriteByte(rest[i])
			}
			val = b.String()
			rest = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			val = strings.TrimSpace(rest[:end])
			rest = rest[end:]
		}
		params[key] = val
		rest = strings.TrimLeft(rest, ", ")
	}
	return scheme, params
}
