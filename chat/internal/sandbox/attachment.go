package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// MaxAttachmentBytes bounds one file an owner sends to the agent.
const MaxAttachmentBytes = 8 << 20

// attachmentPath is the only shape attachment-write puts a file at: the
// chat service names the file, never the sender, so the path cannot leave
// the workspace's attachment directory.
var attachmentPath = regexp.MustCompile(`^\.warden/attachments/[a-f0-9]{32}\.[a-z0-9]{1,8}$`)

// attachmentScript moves a file the runtime copied into the guest's /tmp
// to its place in the workspace. The workspace is the agent's, so it may
// have turned .warden or the attachment directory into a symlink; the move
// refuses those instead of following them, and mv over a symlinked target
// replaces the link (rename semantics), never its referent.
const attachmentScript = `set -eu
staged=$1; target=$2; dir=${target%/*}; top=${dir%/*}
if [ -L "$top" ] || [ -L "$dir" ]; then echo "attachment directory is a symlink" >&2; exit 1; fi
mkdir -p "$dir"
chown agent:agent "$top" "$dir" "$staged"
chmod 644 "$staged"
mv -f "$staged" "$target"
`

// writeAttachmentLocked puts the request's bytes at r.Directory beneath the
// sandbox workspace: staged on the worker host, copied into the guest's /tmp
// by the runtime, then moved into place by attachmentScript.
func (w *Worker) writeAttachmentLocked(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	if !attachmentPath.MatchString(r.Directory) {
		return Response{}, errors.New("invalid attachment path")
	}
	if len(r.Bytes) == 0 || len(r.Bytes) > MaxAttachmentBytes {
		return Response{}, errors.New("attachment must be between 1 byte and 8 MiB")
	}
	name := filepath.Base(r.Directory)
	dir := filepath.Join(w.Root, "attachments")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Response{}, err
	}
	staged := filepath.Join(dir, name)
	if err := os.WriteFile(staged, r.Bytes, 0600); err != nil {
		return Response{}, err
	}
	defer os.Remove(staged)
	guest := "/tmp/warden-attachment-" + name
	if err := w.Runtime.Copy(ctx, s.RuntimeName, staged, guest); err != nil {
		return Response{}, errors.New("could not copy the attachment into the sandbox")
	}
	if _, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "sh", "-c", attachmentScript, "warden-attachment", guest, r.Directory); err != nil {
		return Response{}, errors.New("could not place the attachment in the workspace")
	}
	return Response{Directory: r.Directory}, nil
}
