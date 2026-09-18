package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"warden/chat/internal/config"
)

// holdLauncherLock takes the state's launcher lock on its own descriptor,
// as a running stack does, and returns the release.
func holdLauncherLock(t *testing.T, state string) func() {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(state, "launcher.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

func TestLauncherLockWaitsForThePreviousInstanceWhenDetached(t *testing.T) {
	state := t.TempDir()
	release := holdLauncherLock(t, state)
	defer func() { launcherLockWait = 30 * time.Second }()
	launcherLockWait = 5 * time.Second
	out := &bytes.Buffer{}
	l := &launcher{c: &cli{stdout: out, stderr: out}, cfg: config.Config{}, detached: true}
	go func() {
		time.Sleep(600 * time.Millisecond)
		release()
	}()
	started := time.Now()
	lock, err := l.launcherLock(state)
	if err != nil {
		t.Fatalf("a detached instance should wait for the lock: %v", err)
	}
	defer lock.Close()
	if took := time.Since(started); took < 500*time.Millisecond {
		t.Fatalf("the lock was taken after %s while the previous instance still held it", took)
	}
	if !strings.Contains(out.String(), "waiting for the previous instance to exit") {
		t.Fatalf("the wait was not announced: %q", out.String())
	}
}

func TestLauncherLockGivesUpWhenThePreviousInstanceStays(t *testing.T) {
	state := t.TempDir()
	release := holdLauncherLock(t, state)
	defer release()
	defer func() { launcherLockWait = 30 * time.Second }()
	launcherLockWait = 600 * time.Millisecond
	l := &launcher{c: &cli{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, cfg: config.Config{}, detached: true}
	_, err := l.launcherLock(state)
	if err == nil || !strings.Contains(err.Error(), "did not exit within") {
		t.Fatalf("expected the wait to give up, got %v", err)
	}
}

func TestLauncherLockRefusesAForegroundStartAtOnce(t *testing.T) {
	state := t.TempDir()
	release := holdLauncherLock(t, state)
	defer release()
	l := &launcher{c: &cli{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, cfg: config.Config{}}
	started := time.Now()
	_, err := l.launcherLock(state)
	if err == nil || !strings.Contains(err.Error(), "already running or shutting down") {
		t.Fatalf("expected the foreground refusal, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("a foreground start must not wait for the lock")
	}
}
