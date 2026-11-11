package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var honchoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// Key reads the API key from APIKeyFile, else the variable APIKeyEnv names,
// else APIKey.
func (h Honcho) Key() (string, error) {
	key := h.APIKey
	switch {
	case h.APIKeyFile != "":
		b, err := os.ReadFile(h.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("honcho.api_key_file: %w", err)
		}
		key = string(b)
	case h.APIKeyEnv != "":
		key = os.Getenv(h.APIKeyEnv)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("honcho needs api_key, api_key_file or api_key_env with a value")
	}
	return key, nil
}

func (h *Honcho) validate(i int) error {
	if h.URL == "" {
		return nil
	}
	if _, ok := webURL(h.URL); !ok {
		return fmt.Errorf("%w: backends[%d].honcho.url must be an http or https URL", ErrBackend, i)
	}
	h.URL = strings.TrimRight(h.URL, "/")
	if h.AIPeer == "" {
		h.AIPeer = "hermes"
	}
	for _, f := range [][2]string{{"workspace", h.Workspace}, {"user_peer", h.UserPeer}, {"ai_peer", h.AIPeer}} {
		if !honchoIDPattern.MatchString(f[1]) {
			return fmt.Errorf("%w: backends[%d].honcho.%s must be letters, digits, _ and -", ErrBackend, i, f[0])
		}
	}
	if h.UserPeer == h.AIPeer {
		return fmt.Errorf("%w: backends[%d].honcho.user_peer and ai_peer must differ", ErrBackend, i)
	}
	if _, err := h.Key(); err != nil {
		return fmt.Errorf("%w: backends[%d].%w", ErrBackend, i, err)
	}
	return nil
}
