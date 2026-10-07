package fakerun

import (
	"context"
	"errors"
	"testing"

	"github.com/fl4metf/vm-harness/runner"
)

func TestRulesAndSequences(t *testing.T) {
	ctx := context.Background()
	f := New().
		On("showvminfo", "default").
		OnResult("showvminfo x", runner.Result{Stdout: []byte("first")}, runner.Result{Stdout: []byte("second")}).
		OnResult("startvm", runner.Result{ExitCode: 1, Stderr: []byte("boom")})

	for _, want := range []string{"first", "second", "second"} {
		res, err := f.Run(ctx, "VBoxManage", "showvminfo", "x", "--machinereadable")
		if err != nil || string(res.Stdout) != want {
			t.Fatalf("got %q, %v; want %q", res.Stdout, err, want)
		}
	}
	if res, _ := f.Run(ctx, "VBoxManage", "showvminfo", "y"); string(res.Stdout) != "default" {
		t.Fatalf("fallback rule not used: %q", res.Stdout)
	}
	if res, _ := f.Run(ctx, "VBoxManage", "startvm", "x"); res.ExitCode != 1 || string(res.Stderr) != "boom" {
		t.Fatalf("fail rule: %+v", res)
	}
	if _, err := f.Run(ctx, "VBoxManage", "list", "vms"); err == nil {
		t.Fatal("unmatched command should fail")
	}
	if !f.Called("startvm x") || f.Called("controlvm") || len(f.Find("showvminfo")) != 4 {
		t.Fatalf("call log wrong: %v", f.Calls())
	}
}

func TestOnErrorAndFunc(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("spawn failed")
	f := New().OnError("x", sentinel).OnFunc("echo", func(c Call) (runner.Result, error) {
		return runner.Result{Stdout: []byte(c.Args[1])}, nil
	})
	if _, err := f.Run(ctx, "bin", "x"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if res, _ := f.Run(ctx, "bin", "echo", "hi"); string(res.Stdout) != "hi" {
		t.Fatalf("func rule: %q", res.Stdout)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Run(cctx, "bin", "echo", "hi"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx: %v", err)
	}
}
