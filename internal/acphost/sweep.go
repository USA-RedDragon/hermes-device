package acphost

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Server) untrack(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.children, pid)
}

func (s *Server) sweepLoop() {
	t := time.NewTicker(s.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.base.Done():
			return
		case <-t.C:
		}
		s.sweep()
	}
}

// sweep stops processes that left a child's process group, such as a
// gateway that hermes acp started with its own session, and reaps orphans
// that were reparented to the host.
func (s *Server) sweep() {
	s.mu.Lock()
	strays, zombies := findStrays(os.Getpid(), s.children, s.orphans.Load())
	s.mu.Unlock()
	for _, pid := range zombies {
		reap(pid)
	}
	for _, pid := range strays {
		s.mu.Lock()
		busy := s.killing[pid]
		s.killing[pid] = true
		s.mu.Unlock()
		if busy {
			continue
		}
		s.opts.Logger.Warn("Stopping a process that left its hermes acp process group", "pid", pid)
		go s.stopStray(pid)
	}
}

func (s *Server) stopStray(pid int) {
	defer func() {
		s.mu.Lock()
		delete(s.killing, pid)
		s.mu.Unlock()
	}()
	_ = signalPid(pid, syscall.SIGTERM)
	time.Sleep(s.opts.KillGrace)
	_ = signalPid(pid, syscall.SIGKILL)
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
