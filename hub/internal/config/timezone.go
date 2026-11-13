package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServerTimeZone is the IANA name of the server's time zone: the TZ
// environment variable, else the zone /etc/localtime links to, else UTC.
const utc = "UTC"

func ServerTimeZone() string {
	return serverTimeZone(os.Getenv("TZ"), "/etc/localtime")
}

func serverTimeZone(tz, localtime string) string {
	tz = strings.TrimPrefix(tz, ":")
	if strings.HasPrefix(tz, "/") {
		localtime = tz
	} else if tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	target, err := filepath.EvalSymlinks(localtime)
	if err != nil {
		return utc
	}
	_, name, ok := strings.Cut(filepath.ToSlash(target), "/zoneinfo/")
	if !ok {
		return utc
	}
	if _, err := time.LoadLocation(name); err != nil {
		return utc
	}
	return name
}
