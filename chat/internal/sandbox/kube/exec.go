package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// execOutputLimit caps what Exec returns, as the SBX driver caps it.
const execOutputLimit = 96 << 20

// execStartAttempts is how many times an exec refused by the server right
// after the container started (the runtime has not registered it yet) is
// retried, execRetry apart.
const execStartAttempts = 12

// exitError is a command that ended with a non-zero status. Its stderr is
// agent-controlled and never part of the message.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("sandbox exec failed: exit status %d", e.code) }

// run executes command in the guest container with stdin (nil for none),
// returns its stdout up to limit bytes (execOutputLimit when zero) and an
// *exitError on a non-zero exit. Stderr is drained and discarded. A
// cancelled ctx is the result whatever the command reported, and a
// command that ends before reading all its input reports its exit status,
// not the closed socket.
func (d *Driver) run(ctx context.Context, name string, stdin io.Reader, limit int64, command ...string) ([]byte, error) {
	if limit <= 0 {
		limit = execOutputLimit
	}
	session, err := d.open(ctx, name, command, stdin != nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	var wg sync.WaitGroup
	var out bytes.Buffer
	var outErr, inErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, err := io.Copy(&out, io.LimitReader(session.Stdout(), limit+1))
		if err == nil && n > limit {
			err = errors.New("command output exceeds limit")
		}
		outErr = err
		if err != nil {
			session.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, session.Stderr())
	}()
	if stdin != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(session.Stdin(), stdin); err != nil {
				inErr = err
				return
			}
			_ = session.Stdin().Close()
		}()
	}
	code, err := session.Wait()
	if ctx.Err() != nil {
		session.Close()
		wg.Wait()
		return nil, ctx.Err()
	}
	wg.Wait()
	if outErr != nil {
		return nil, outErr
	}
	if err != nil {
		if inErr != nil {
			return nil, fmt.Errorf("sandbox %s: exec input: %w", name, inErr)
		}
		return nil, fmt.Errorf("sandbox %s: exec: %w", name, err)
	}
	if code != 0 {
		return nil, &exitError{code}
	}
	return out.Bytes(), nil
}

// open starts an exec session, retrying a refusal that follows a fresh
// container start.
func (d *Driver) open(ctx context.Context, name string, command []string, stdin bool) (*kube.Session, error) {
	var last error
	for attempt := 0; attempt < execStartAttempts; attempt++ {
		session, err := d.client.Exec(ctx, d.opts.Namespace, name, ContainerName, command, kube.ExecOptions{Stdin: stdin})
		if err == nil {
			return session, nil
		}
		last = err
		if ctx.Err() != nil || kube.IsNotFound(err) || kube.IsForbidden(err) || kube.IsUnauthorized(err) || !isStatus(err) {
			break
		}
		if !sleep(ctx, d.execRetry) {
			break
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("sandbox %s: exec: %w", name, last)
}

func sleep(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// inDirectory wraps a command so it runs in dir: pods/exec has no working
// directory of its own. dir and the command travel as arguments of a
// fixed script, never as shell text.
func inDirectory(dir string, args []string) []string {
	return append([]string{"sh", "-c", `cd "$0" && exec "$@"`, dir}, args...)
}

// Exec runs args in the guest at dir and returns its stdout.
func (d *Driver) Exec(ctx context.Context, name, dir string, args ...string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "", errors.New("sandbox exec needs a command")
	}
	if dir == "" {
		dir = "/"
	}
	out, err := d.run(ctx, name, nil, 0, inDirectory(dir, args)...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Copy sends a host file or directory into the guest at target, as sbx cp
// does: a tar stream of the source, named after target's base, is extracted
// by root into target's directory, so the guest receives root-owned files
// the worker adjusts afterwards (chmod, chown) as it does with SBX.
func (d *Driver) Copy(ctx context.Context, name, source, target string) error {
	if err := validName(name); err != nil {
		return err
	}
	if !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
		return fmt.Errorf("copy target %q must be a clean absolute path", target)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("copy source %q must be a file or a directory", source)
	}
	reader, writer := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		err := writeArchive(writer, source, path.Base(target))
		archiveErr <- err
		_ = writer.CloseWithError(err)
	}()
	_, err = d.run(ctx, name, reader, 0, "sudo", "-n", "sh", "-c", `mkdir -p "$0" && tar -xf - -C "$0"`, path.Dir(target))
	_ = reader.CloseWithError(errors.New("copy ended"))
	if aerr := <-archiveErr; aerr != nil && err == nil {
		err = aerr
	}
	if err != nil {
		return fmt.Errorf("sandbox %s: copy to %s: %w", name, target, err)
	}
	return nil
}

// CopyOut is the reverse of Copy on the sbx shapes: a guest directory back
// to the owner's machine. The runner is a pod here and holds no directory
// of the owner's (the chat offers none: LocalMode is off in this kind), so
// there is nowhere to copy to.
func (d *Driver) CopyOut(context.Context, string, string, string) error {
	return errors.New("no host directory on Kubernetes: the runner is a pod")
}

// writeArchive writes source as a tar stream whose root entry is named
// base: one file entry, or the directory and its contents. Entries are
// root-owned; modes are the host's.
func writeArchive(w io.Writer, source, base string) error {
	tw := tar.NewWriter(w)
	add := func(hostPath, archivePath string, info fs.FileInfo) error {
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			var err error
			if link, err = os.Readlink(hostPath); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = archivePath
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "root", "root"
		header.Format = tar.FormatPAX
		if err = tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(hostPath)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err = add(source, base, info); err != nil {
			return err
		}
		return tw.Close()
	}
	err = filepath.WalkDir(source, func(hostPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil // sockets, devices and the like are not copied
		}
		rel, err := filepath.Rel(source, hostPath)
		if err != nil {
			return err
		}
		archivePath := base
		if rel != "." {
			archivePath = path.Join(base, filepath.ToSlash(rel))
		}
		return add(hostPath, archivePath, info)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// copyHome copies the source runtime's home into the target's through
// exec (decision 8's fallback when the storage does not clone): a tar of
// the source pod's home streamed into the target pod's. Both pods must be
// running; the guest account owns both homes.
func (d *Driver) copyHome(ctx context.Context, source, target string) error {
	home := d.opts.home()
	from, err := d.open(ctx, source, []string{"tar", "-C", home, "-cf", "-", "."}, false)
	if err != nil {
		return fmt.Errorf("fork of %s: source pod is not running: %w", source, err)
	}
	defer from.Close()
	go func() { _, _ = io.Copy(io.Discard, from.Stderr()) }()
	if _, err = d.run(ctx, target, from.Stdout(), 0, "tar", "-C", home, "-xf", "-"); err != nil {
		return fmt.Errorf("fork of %s: %w", source, err)
	}
	if code, err := from.Wait(); err != nil || code != 0 {
		if err == nil {
			err = &exitError{code}
		}
		return fmt.Errorf("fork of %s: reading its home: %w", source, err)
	}
	return nil
}

// Stream launches the agent app server in the guest: the shared launch
// line (sandbox.AgentCommand) in the run's directory, with the tier's
// options. Trust needs no step: the guest mounts its bundle (decision 9).
// The stream's reader ends with EOF when the agent exits or ctx is
// cancelled; Close ends the session.
func (d *Driver) Stream(ctx context.Context, name string, run sandbox.RunSpec) (io.ReadWriteCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	dir := run.Directory
	if dir == "" {
		dir = d.opts.home()
	}
	ctx, cancel := context.WithCancel(ctx)
	session, err := d.open(ctx, name, inDirectory(dir, sandbox.AgentCommand(run, d.launchOptions())), true)
	if err != nil {
		cancel()
		return nil, errors.New("could not start sandbox app server")
	}
	// Agent-controlled stderr must not enter controller service logs.
	go func() { _, _ = io.Copy(io.Discard, session.Stderr()) }()
	return &execStream{session: session, cancel: cancel}, nil
}

// execStream adapts a Session to the worker's stream.
type execStream struct {
	session *kube.Session
	cancel  context.CancelFunc
	once    sync.Once
}

func (s *execStream) Read(b []byte) (int, error)  { return s.session.Stdout().Read(b) }
func (s *execStream) Write(b []byte) (int, error) { return s.session.Stdin().Write(b) }
func (s *execStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.session.Close()
	})
	return nil
}
