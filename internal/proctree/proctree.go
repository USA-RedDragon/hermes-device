// Package proctree runs a command as the head of its own process tree and
// stops the whole tree: a process group on Unix, a Job Object on Windows.
//
// Use: set cmd.SysProcAttr = SysProcAttr() (or call Prepare), start the
// command, then call Attach right away. Terminate and Kill reach every
// process the command started that is still in the tree. Release lets go of
// the tree after the head exited on its own: processes it left running
// keep running, as they do on Unix.
package proctree
