package chats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// Files from the owner's own machine, attached from the web composer the
// way the TUI attaches them from its host (docs/web-attach-from-disk-plan.md):
// /attach PATH… and a local mention (@./x, @../x, @~/x) name a path the
// chat service reads, since on a local install it runs on the owner's
// machine as their user. Only there: LocalMode is a single-owner install
// that is not the Kubernetes shape, where the service's filesystem is a
// pod's. A relative path resolves against the home directory (a browser
// tab has no working directory).

// localPathLimit bounds a completion listing, as the workspace listing is.
const localPathLimit = 50

// errNoLocalFiles is why a path cannot be read outside a local install.
var errNoLocalFiles = errors.New("files from this computer are only available on a local Warden install")

// localFiles reports whether this request may read the owner's files.
func (h *HTTP) localFiles(r *http.Request) bool {
	return h.Engine.LocalMode && isOwner(r)
}

// homeDir is the directory ~ and a relative path resolve against.
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/"
}

// ExpandLocalPath makes a typed path absolute: ~ is the home directory,
// as is the base of a relative path.
func ExpandLocalPath(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "~":
		return homeDir()
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(homeDir(), p[2:])
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	}
	return filepath.Join(homeDir(), p)
}

// LocalPaths completes a typed path, shell style: the entries of the
// directory the query names whose name starts with its last segment (case
// folded), hidden ones only when the segment names them, directories
// first with a trailing slash, each written the way the query was.
func LocalPaths(query string) []string {
	dir, stem := filepath.Split(query)
	lookup := ExpandLocalPath(dir)
	entries, err := os.ReadDir(lookup)
	if err != nil {
		return nil
	}
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(stem)) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(stem, ".") {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(lookup, name)); err == nil {
				isDir = info.IsDir()
			}
		}
		if isDir {
			dirs = append(dirs, dir+name+"/")
		} else {
			files = append(files, dir+name)
		}
	}
	fold := func(s []string) {
		sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i]) < strings.ToLower(s[j]) })
	}
	fold(dirs)
	fold(files)
	out := append(dirs, files...)
	if len(out) > localPathLimit {
		out = out[:localPathLimit]
	}
	return out
}

// localPathsHTTP answers GET chats/{id}/local-paths?q=~/Doc with
// {"paths": [...]} from this machine.
func (h *HTTP) localPathsHTTP(w http.ResponseWriter, r *http.Request, chatID string) {
	if !h.localFiles(r) {
		http.Error(w, errNoLocalFiles.Error(), 403)
		return
	}
	query := r.URL.Query().Get("q")
	if len(query) > sandbox.MaxPathQuery {
		http.Error(w, "path query too long", 400)
		return
	}
	if h.Engine.Store.Chat(chatID) == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	paths := LocalPaths(query)
	if paths == nil {
		paths = []string{}
	}
	json.NewEncoder(w).Encode(map[string][]string{"paths": paths})
}

// LocalAttachResult is what attaching typed paths comes to: the records
// stored, in the order the paths matched, each with the path it was typed
// as (a mention is rewritten by it), one line per path or match that
// could not be attached, and the typed paths that named no file or a
// directory (a mention of one is sent as written, as the TUI sends it).
type LocalAttachResult struct {
	Attached []LocalAttached `json:"attached"`
	Errors   []string        `json:"errors"`
	Missing  []string        `json:"missing"`
}

// LocalAttached is a stored record with the path that named it.
type LocalAttached struct {
	cv.Attachment
	Typed string `json:"typed"`
}

// AttachLocal attaches the files the paths name for the chat's next
// message: each path ~-expanded and globbed on this machine; a pattern
// that matches nothing, a directory, an empty or oversized file, and a
// read that fails are errors by name. limit is how many more attachments
// the message can take (the client knows its pending uploads); reaching
// it stops the batch with an error saying so.
func (e *Engine) AttachLocal(ctx context.Context, c *Chat, paths []string, limit int) LocalAttachResult {
	res := LocalAttachResult{Attached: []LocalAttached{}, Errors: []string{}, Missing: []string{}}
	if limit < 0 {
		limit = 0
	}
	for _, typed := range paths {
		typed = strings.TrimSpace(typed)
		if typed == "" {
			continue
		}
		expanded := ExpandLocalPath(typed)
		matches := []string{expanded}
		if strings.ContainsAny(typed, "*?[") {
			if m, err := filepath.Glob(expanded); err == nil && len(m) > 0 {
				sort.Strings(m)
				matches = m
			} else {
				res.Missing = append(res.Missing, typed)
				res.Errors = append(res.Errors, typed+": matches nothing")
				continue
			}
		}
		for _, path := range matches {
			if len(res.Attached) >= limit {
				res.Errors = append(res.Errors, fmt.Sprintf("a message can carry at most %d attachments; %s was not attached", maxMessageAttachments, shown(typed, path, expanded)))
				return res
			}
			a, err := e.attachLocalFile(ctx, c, path)
			if err != nil {
				if errors.Is(err, errMissingFile) || errors.Is(err, errIsDirectory) {
					res.Missing = append(res.Missing, typed)
				}
				res.Errors = append(res.Errors, shown(typed, path, expanded)+": "+err.Error())
				continue
			}
			res.Attached = append(res.Attached, LocalAttached{Attachment: a, Typed: typed})
		}
	}
	return res
}

// shown names a match the way the person typed it: the typed path for a
// plain one; a glob's match under the directory it was typed in (~/x/*.log
// gives ~/x/a.log), or the match's own path when the directory itself
// held the pattern.
func shown(typed, path, expanded string) string {
	if path == expanded {
		return typed
	}
	dir, _ := filepath.Split(typed)
	if strings.ContainsAny(dir, "*?[") {
		return path
	}
	if rel, err := filepath.Rel(ExpandLocalPath(dir), path); err == nil && !strings.HasPrefix(rel, "..") {
		return dir + rel
	}
	return path
}

// errMissingFile and errIsDirectory mark a path that names no file.
var (
	errMissingFile = errors.New("no such file")
	errIsDirectory = errors.New("is a directory; attach a file")
)

// attachLocalFile stores one file from this machine as an attachment.
func (e *Engine) attachLocalFile(ctx context.Context, c *Chat, path string) (cv.Attachment, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return cv.Attachment{}, errMissingFile
	case err != nil:
		return cv.Attachment{}, errors.New("cannot be read")
	case info.IsDir():
		return cv.Attachment{}, errIsDirectory
	case info.Size() == 0:
		return cv.Attachment{}, errors.New("is empty")
	case info.Size() > sandbox.MaxAttachmentBytes:
		return cv.Attachment{}, errors.New("is larger than 8 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cv.Attachment{}, errors.New("cannot be read")
	}
	return e.storeAttachment(ctx, c, filepath.Base(path), data)
}

// attachLocalHTTP answers POST chats/{id}/attach-local {"paths": [...],
// "limit": n} with a LocalAttachResult.
func (h *HTTP) attachLocalHTTP(w http.ResponseWriter, r *http.Request, chatID string) {
	if !h.localFiles(r) {
		http.Error(w, errNoLocalFiles.Error(), 403)
		return
	}
	c := h.Engine.Store.Chat(chatID)
	if c == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	if c.Archived {
		respond(w, nil, errors.New("chat is archived"))
		return
	}
	var body struct {
		Paths []string `json:"paths"`
		Limit *int     `json:"limit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || len(body.Paths) == 0 {
		http.Error(w, "paths required", 400)
		return
	}
	limit := maxMessageAttachments
	if body.Limit != nil && *body.Limit < limit {
		limit = *body.Limit
	}
	for _, p := range body.Paths {
		if len(p) > sandbox.MaxPathQuery {
			http.Error(w, "path too long", 400)
			return
		}
	}
	json.NewEncoder(w).Encode(h.Engine.AttachLocal(r.Context(), c, body.Paths, limit))
}
