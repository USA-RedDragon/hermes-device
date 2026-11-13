package acphost

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const prSetChildSubreaper = 36

// becomeSubreaper makes orphaned descendants of hermes acp children of the
// host, so the sweep can stop and reap them.
func becomeSubreaper() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return errno
	}
	return nil
}

// findStrays reads /proc. A stray is a live descendant of a tracked child
// whose process group is not the child's, or, when orphans is set, a live
// process reparented to self that is not a tracked child. Zombies
// reparented to self are then returned for reaping.
func findStrays(self int, children map[int]bool, orphans bool) (strays, zombies []int) {
	dirs, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, nil
	}
	procs := make(map[int]procInfo, len(dirs))
	for _, f := range dirs {
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(f)))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		info, ok := parseStat(string(data))
		if ok {
			procs[pid] = info
		}
	}
	for pid, info := range procs {
		if children[pid] {
			continue
		}
		if orphans && info.ppid == self {
			if info.zombie {
				zombies = append(zombies, pid)
			} else {
				strays = append(strays, pid)
			}
			continue
		}
		if info.zombie {
			continue
		}
		anc := info.ppid
		for range 64 {
			if children[anc] {
				if info.pgid != anc {
					strays = append(strays, pid)
				}
				break
			}
			p, ok := procs[anc]
			if !ok || anc <= 1 {
				break
			}
			anc = p.ppid
		}
	}
	return strays, zombies
}

type procInfo struct {
	ppid, pgid int
	zombie     bool
}

// parseStat reads state, ppid and pgrp from a /proc/<pid>/stat line. The
// command name is in parentheses and may itself hold spaces or parentheses.
func parseStat(line string) (procInfo, bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(line[i+1:])
	if len(f) < 3 {
		return procInfo{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgid, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return procInfo{}, false
	}
	return procInfo{ppid: ppid, pgid: pgid, zombie: f[0] == "Z"}, true
}
