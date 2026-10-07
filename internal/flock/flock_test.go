package flock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcludesOtherHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		second, err := Lock(context.Background(), path)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- second
	}()
	select {
	case <-got:
		t.Fatal("second holder got the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case second, ok := <-got:
		if ok {
			second()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second holder did not get the lock after it was released")
	}
}

func TestLockHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Lock(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited past the deadline")
	}
}

func TestLockCanBeTakenAgainAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	for range 3 {
		unlock, err := Lock(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
}

func TestLockMissingDirectory(t *testing.T) {
	if _, err := Lock(t.Context(), filepath.Join(t.TempDir(), "missing", "x.lock")); err == nil {
		t.Fatal("lock in a missing directory succeeded")
	}
}
