//go:build !linux

package acphost

func becomeSubreaper() error { return nil }

func findStrays(int, map[int]bool, bool) (strays, zombies []int) { return nil, nil }
