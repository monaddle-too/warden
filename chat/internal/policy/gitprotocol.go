package policy

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ZeroOID is the all-zero SHA-1 used for absent refs.
const ZeroOID = "0000000000000000000000000000000000000000"

var gitRoutePattern = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9_.-]{1,100})\.git/(info/refs\?service=git-(upload|receive)-pack|git-(upload|receive)-pack)$`)
var gitPacketLength = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)
var gitAgentCap = regexp.MustCompile(`^agent=[A-Za-z0-9._/+()-]{1,100}$`)
var gitCommandLine = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (refs/heads/[A-Za-z0-9_./-]{1,200})$`)

// GitRoute describes a recognised smart HTTP endpoint.
type GitRoute struct {
	Repository string
	Write      bool
	Discovery  bool
}

// GitUpdate is the single branch update carried by an accepted push.
type GitUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// GitRouteFor recognises upload-pack discovery/service and receive-pack.
func GitRouteFor(method, path string) *GitRoute {
	m := gitRoutePattern.FindStringSubmatch(path)
	if m == nil || m[2] == "." || m[2] == ".." {
		return nil
	}
	discovery := strings.HasPrefix(m[3], "info/")
	want := "POST"
	if discovery {
		want = "GET"
	}
	if method != want {
		return nil
	}
	return &GitRoute{Repository: m[1] + "/" + m[2], Write: !discovery && m[5] == "receive", Discovery: discovery}
}

// GitPacket reads one pkt-line at offset. A flush packet returns nil data.
func GitPacket(data []byte, offset int) ([]byte, int, error) {
	if offset+4 > len(data) || !gitPacketLength.Match(data[offset:offset+4]) {
		return nil, 0, errors.New("invalid Git packet length")
	}
	size64, _ := strconv.ParseInt(string(data[offset:offset+4]), 16, 32)
	size := int(size64)
	if size == 0 {
		return nil, offset + 4, nil
	}
	if size < 4 || size > 65520 || offset+size > len(data) {
		return nil, 0, errors.New("invalid Git packet bounds")
	}
	return data[offset+4 : offset+size], offset + size, nil
}

// GitPush parses a receive-pack request: exactly one SHA-1 branch update
// with a small capability set, followed by the pack bytes.
func GitPush(data []byte) (GitUpdate, []byte, error) {
	command, offset, err := GitPacket(data, 0)
	if err != nil {
		return GitUpdate{}, nil, err
	}
	if command == nil || !bytes.Contains(command, []byte{0}) {
		return GitUpdate{}, nil, errors.New("Git push requires an explicit capability list")
	}
	parts := bytes.SplitN(command, []byte{0}, 2)
	line, caps := parts[0], parts[1]
	caps = bytes.TrimSuffix(caps, []byte("\n"))
	caps = bytes.TrimPrefix(caps, []byte(" "))
	for _, cap := range bytes.Split(caps, []byte(" ")) {
		switch string(cap) {
		case "report-status", "report-status-v2", "side-band-64k", "quiet", "ofs-delta", "atomic", "object-format=sha1":
			continue
		}
		if gitAgentCap.Match(cap) {
			continue
		}
		return GitUpdate{}, nil, errors.New("unsupported Git push capability")
	}
	m := gitCommandLine.FindSubmatch(line)
	if m == nil {
		return GitUpdate{}, nil, errors.New("only SHA-1 branch pushes are supported")
	}
	update := GitUpdate{Old: string(m[1]), New: string(m[2]), Ref: string(m[3])}
	if update.New == ZeroOID || update.New == update.Old {
		return GitUpdate{}, nil, errors.New("branch deletion and empty updates are unsupported")
	}
	for _, part := range strings.Split(update.Ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return GitUpdate{}, nil, errors.New("invalid Git branch name")
		}
	}
	if strings.Contains(update.Ref, "..") || strings.Contains(update.Ref, "@{") {
		return GitUpdate{}, nil, errors.New("invalid Git branch name")
	}
	end, offset, err := GitPacket(data, offset)
	if err != nil {
		return GitUpdate{}, nil, err
	}
	if end != nil {
		return GitUpdate{}, nil, errors.New("push one branch at a time; multiple ref updates are blocked")
	}
	pack := data[offset:]
	if len(pack) > 0 && (!bytes.HasPrefix(pack, []byte("PACK")) || len(pack) < 32) {
		return GitUpdate{}, nil, errors.New("invalid Git pack")
	}
	return update, pack, nil
}

// GitDecodeBody undoes the transfer compression git applies to any RPC
// body over 1 KiB (a clone of a repository with a few dozen refs already
// crosses it): a gzip body is inflated, to at most limit bytes, and the
// Content-Encoding header dropped so the inspected, forwarded request is
// the identity form GitHub accepts equally. Other encodings are left for
// GitInspect to refuse.
func GitDecodeBody(headers [][]string, body []byte, limit int) ([][]string, []byte, error) {
	encoding := ""
	for _, pair := range headers {
		if len(pair) == 2 && strings.EqualFold(pair[0], "content-encoding") {
			encoding = strings.ToLower(strings.TrimSpace(pair[1]))
		}
	}
	if encoding != "gzip" && encoding != "x-gzip" {
		return headers, body, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("invalid compressed Git request")
	}
	inflated, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, nil, errors.New("invalid compressed Git request")
	}
	if len(inflated) > limit {
		return nil, nil, errors.New("request exceeds inspection limit")
	}
	kept := make([][]string, 0, len(headers))
	for _, pair := range headers {
		if len(pair) == 2 && strings.EqualFold(pair[0], "content-encoding") {
			continue
		}
		kept = append(kept, pair)
	}
	return kept, inflated, nil
}

// GitInspection is the result of validating a smart HTTP request.
type GitInspection struct {
	GitRoute
	Update *GitUpdate
}

// GitInspect validates method, path, headers and body of a Git request.
func GitInspect(method, path string, headers [][]string, body []byte) (*GitInspection, error) {
	route := GitRouteFor(method, path)
	if route == nil {
		return nil, errors.New("unsupported Git HTTP endpoint; use an HTTPS .git remote")
	}
	lower := map[string]string{}
	for _, pair := range headers {
		if len(pair) == 2 {
			lower[strings.ToLower(pair[0])] = pair[1]
		}
	}
	if encoding, ok := lower["content-encoding"]; ok && encoding != "identity" {
		return nil, errors.New("compressed Git requests are unsupported")
	}
	result := &GitInspection{GitRoute: *route}
	if route.Discovery {
		if len(body) > 0 {
			return nil, errors.New("Git discovery must not contain a body")
		}
		return result, nil
	}
	service := "upload"
	if route.Write {
		service = "receive"
	}
	if lower["content-type"] != "application/x-git-"+service+"-pack-request" {
		return nil, errors.New("invalid Git content type")
	}
	if len(body) == 0 {
		return nil, errors.New("empty Git service request")
	}
	if route.Write {
		update, _, err := GitPush(body)
		if err != nil {
			return nil, err
		}
		result.Update = &update
	}
	return result, nil
}

func isHex40(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
