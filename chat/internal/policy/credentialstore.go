package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// CredentialStore keeps provider logins (docs/warden-kubernetes-plan.md,
// decision 8): owner-only files on the sbx shapes, Kubernetes Secrets in
// that kind. Names are store-specific (a file path, a Secret name). Loaders
// read on every use so a new sign-in takes effect without a restart; Store
// is for a refreshed login written back; Watch reports rotations.
type CredentialStore interface {
	// Load returns the credential named, or an error when it is missing,
	// unreadable or not private.
	Load(ctx context.Context, name string) ([]byte, error)
	// Store replaces the credential named atomically, keeping it private.
	Store(ctx context.Context, name string, data []byte) error
	// Watch reports every change of the credential named on the returned
	// channel (one signal per change, coalesced) until ctx ends, when the
	// channel is closed.
	Watch(ctx context.Context, name string) (<-chan struct{}, error)
}

// FileCredentials is the CredentialStore of the sbx shapes: a name is the
// absolute path of an owner-only 0600 regular file, read without following
// a final symlink and bounded by Limit, with Description naming it in
// errors, exactly as the loaders read their files before the store existed.
type FileCredentials struct {
	// Limit is the largest credential accepted, 1 MiB when zero.
	Limit int64
	// Description names the credential in errors ("Codex credential file").
	Description string
	// PollInterval is how often Watch inspects the file, one second when
	// zero.
	PollInterval time.Duration
}

func (f FileCredentials) limit() int64 {
	if f.Limit > 0 {
		return f.Limit
	}
	return 1024 * 1024
}

func (f FileCredentials) description() string {
	if f.Description != "" {
		return f.Description
	}
	return "credential file"
}

// Load reads the private file at name.
func (f FileCredentials) Load(_ context.Context, name string) ([]byte, error) {
	if !filepath.IsAbs(name) {
		return nil, errors.New(f.description() + " must be an absolute path")
	}
	return openPrivate(name, f.limit(), f.description())
}

// Store writes name privately and atomically (temporary file beside it,
// fsync, rename, directory fsync) so a reader sees the old or the new
// login, never a partial one. An existing file must already be private.
func (f FileCredentials) Store(_ context.Context, name string, data []byte) error {
	if !filepath.IsAbs(name) {
		return errors.New(f.description() + " must be an absolute path")
	}
	if int64(len(data)) > f.limit() {
		return errors.New(f.description() + " exceeds its size limit")
	}
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New(f.description() + " must be private and owned")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWrite(name, name+".tmp", data)
}

// Watch polls the file's identity (size, modification time, inode) and
// signals each change, including the file appearing or disappearing.
func (f FileCredentials) Watch(ctx context.Context, name string) (<-chan struct{}, error) {
	if !filepath.IsAbs(name) {
		return nil, errors.New(f.description() + " must be an absolute path")
	}
	interval := f.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	changes := make(chan struct{}, 1)
	last := fileIdentity(name) // taken before returning, so nothing between is missed
	go func() {
		defer close(changes)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current := fileIdentity(name)
				if current != last {
					last = current
					select {
					case changes <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return changes, nil
}

// fileIdentity is what a rotation changes: size, mtime and (through
// Sys) the inode a rename installs. Absent files compare equal to each
// other.
func fileIdentity(name string) [3]int64 {
	info, err := os.Lstat(name)
	if err != nil {
		return [3]int64{-1, -1, -1}
	}
	return [3]int64{info.Size(), info.ModTime().UnixNano(), int64(inodeOf(info))}
}

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}
