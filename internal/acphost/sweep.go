package acphost

import (
	"os"
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
