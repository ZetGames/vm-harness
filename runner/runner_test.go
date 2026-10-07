package runner

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("RUNNER_HELPER") {
	case "out":
		os.Stdout.WriteString("hello")
		os.Stderr.WriteString("oops")
		os.Exit(3)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("RUNNER_HELPER", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	exe := helper(t, "out")
	res, err := Exec{}.Run(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "hello" || string(res.Stderr) != "oops" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestExecMissingBinary(t *testing.T) {
	_, err := Exec{}.Run(context.Background(), "vmh-definitely-not-a-binary")
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := err.(*exec.ExitError); ok {
		t.Fatal("missing binary must not look like an exit status")
	}
}

func TestExecHonoursContext(t *testing.T) {
	exe := helper(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Exec{}.Run(ctx, exe)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("process was not killed")
	}
}
