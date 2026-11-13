package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/webpush"
)

// Notify configures notices on people's phones: approvals and questions
// that wait for them, finished long turns, scheduled job results and
// failures. They go to each person's linked Telegram chat (with
// telegram.enabled) and, with a VAPID key, to their browsers by Web Push.
type Notify struct {
	VAPIDPrivateKey     string        `name:"vapid_private_key" secret:"true" description:"Web Push (VAPID) private key: a P-256 key as a PEM block (openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt) or the raw 32 bytes in base64url. Empty with no vapid_private_key_file: no Web Push. Keep it: a new key ends every browser's subscription"`
	VAPIDPrivateKeyFile string        `name:"vapid_private_key_file" description:"File that holds notify.vapid_private_key. Wins over vapid_private_key"`
	VAPIDSubject        string        `name:"vapid_subject" description:"How push services can reach the hub's operator: a mailto: or https: URL. Default http.public_url when that is https. With neither, the hub logs a warning and runs without Web Push"`
	RemindAfter         time.Duration `name:"remind_after" default:"5m" description:"How long an approval or question waits before the hub reminds its person once. 0 is the default"`
	WatchInterval       time.Duration `name:"watch_interval" default:"15s" description:"How often the hub looks for running chats that nobody has open, to see their approvals and questions. 0 is the default"`
	JobInterval         time.Duration `name:"job_interval" default:"1m" description:"How often the hub reads each backend's scheduled jobs for their results and failures. 0 is the default"`
}

// Defaults of the notify durations, also for a zero value.
const (
	defaultRemindAfter   = 5 * time.Minute
	defaultWatchInterval = 15 * time.Second
	defaultJobInterval   = time.Minute
)

// ErrNotify is an invalid notify setting.
var ErrNotify = errors.New("invalid notify settings")

// PushKey reads the VAPID key from VAPIDPrivateKeyFile, else
// VAPIDPrivateKey. It reports false when neither is set.
func (n Notify) PushKey() (*webpush.VAPID, bool, error) {
	text := n.VAPIDPrivateKey
	if n.VAPIDPrivateKeyFile != "" {
		b, err := os.ReadFile(n.VAPIDPrivateKeyFile)
		if err != nil {
			return nil, false, fmt.Errorf("%w: notify.vapid_private_key_file: %w", ErrNotify, err)
		}
		text = string(b)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false, nil
	}
	k, err := webpush.ParseKey(text)
	if err != nil {
		return nil, false, fmt.Errorf("%w: notify.vapid_private_key: %w", ErrNotify, err)
	}
	return k, true, nil
}

func (c *Config) validateNotify() error {
	n := &c.Notify
	if n.RemindAfter < 0 || n.WatchInterval < 0 || n.JobInterval < 0 {
		return fmt.Errorf("%w: notify.remind_after, watch_interval and job_interval must not be negative", ErrNotify)
	}
	if n.RemindAfter == 0 {
		n.RemindAfter = defaultRemindAfter
	}
	if n.WatchInterval == 0 {
		n.WatchInterval = defaultWatchInterval
	}
	if n.JobInterval == 0 {
		n.JobInterval = defaultJobInterval
	}
	if n.VAPIDSubject == "" && strings.HasPrefix(c.HTTP.PublicURL, "https:") {
		n.VAPIDSubject = c.HTTP.PublicURL
	}
	_, pushOn, err := n.PushKey()
	if err != nil {
		return err
	}
	if pushOn && n.VAPIDSubject != "" && !strings.HasPrefix(n.VAPIDSubject, "mailto:") && !strings.HasPrefix(n.VAPIDSubject, "https:") {
		return fmt.Errorf("%w: notify.vapid_subject must be a mailto: or https: URL", ErrNotify)
	}
	return nil
}
