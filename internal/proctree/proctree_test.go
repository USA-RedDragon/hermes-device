package proctree_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/proctree"
)

const helperEnv = "TALARIA_PROCTREE_HELPER"

// TestMain lets the test binary act as the helper processes: "parent"
// starts a "child" and prints its pid; both then wait to be killed.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "parent":
		child := exec.Command(os.Args[0]) //nolint:gosec,noctx // the test binary itself
		child.Env = append(os.Environ(), helperEnv+"=child")
		if err := child.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(child.Process.Pid)
		if os.Getenv(helperEnv+"_EXIT") != "" {
			os.Exit(0)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// start runs the helper parent in a tree and returns it with its child's
// pid.
func start(t *testing.T, exit bool) (*exec.Cmd, *proctree.Tree, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0]) //nolint:gosec,noctx // the test binary itself
	cmd.Env = append(os.Environ(), helperEnv+"=parent")
	if exit {
		cmd.Env = append(cmd.Env, helperEnv+"_EXIT=1")
	}
	proctree.Prepare(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := proctree.Attach(cmd)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read the child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("helper said %q", line)
	}
	return cmd, tree, pid
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still runs", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKillEndsTheWholeTree(t *testing.T) {
	t.Parallel()
	cmd, tree, child := start(t, false)
	if !alive(child) {
		t.Fatal("the child did not start")
	}
	if err := tree.Kill(); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if !proctree.Killed(err) {
		t.Errorf("Killed(%v) = false after Kill", err)
	}
	waitGone(t, child)
	tree.Release()
}

func TestReleaseKeepsWhatIsLeft(t *testing.T) {
	t.Parallel()
	cmd, tree, child := start(t, true)
	if err := cmd.Wait(); err != nil || proctree.Killed(err) {
		t.Fatalf("parent ended with %v", err)
	}
	tree.Release()
	time.Sleep(200 * time.Millisecond)
	if !alive(child) {
		t.Fatal("Release ended the child that the parent left running")
	}
	p, err := os.FindProcess(child)
	if err == nil {
		_ = p.Kill()
	}
	waitGone(t, child)
}
