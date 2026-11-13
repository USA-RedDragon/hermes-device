//go:build windows

package cmd

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// mozillaHosts is where Firefox looks up native messaging hosts for the
// user.
const mozillaHosts = `Software\Mozilla\NativeMessagingHosts\`

// registerHost points Firefox at the manifest: HKCU\...\<name> with the
// manifest's path as its default value. No administrator rights needed.
func registerHost(name, manifest string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, mozillaHosts+name, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", manifest)
}

func unregisterHost(name string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, mozillaHosts+name)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
