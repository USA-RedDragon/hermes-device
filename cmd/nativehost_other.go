//go:build !windows

package cmd

// registerHost does nothing: Firefox finds the manifest by its folder.
func registerHost(string, string) error { return nil }

func unregisterHost(string) error { return nil }
