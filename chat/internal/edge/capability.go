package edge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The owner capability on the Kubernetes shape.
//
// On the single-host shapes the chat mints the owner capability at every
// start, writes it to <state>/app/endpoint.json and the edge reads that
// file: to identify the owner's browser and as its bearer towards the chat.
// Over a tls:// upstream the chat writes no such file and the edge holds no
// bearer for it (its certificate is the credential), but the browser still
// signs in with a capability from somewhere. So there the edge is the
// authority: it mints one at every start, on the chat's own schedule (a
// restart is the rotation, so a stale browser cannot outlive one), persists
// it in its own state directory in the file shape the chat uses (so the
// existing reader, and `warden open`, understand it) and logs the launch
// URL, the one place an operator can read it from.

// MintsOwnerCapability reports whether this edge holds the owner capability
// itself: owner mode over a tls:// upstream. On the loopback shape the chat
// holds it and the edge only reads the chat's file.
func (s *Server) MintsOwnerCapability() bool {
	return s.mint
}

// RotateOwnerCapability mints a fresh owner capability, persists it at
// Config.OwnerTokenFile (0600, {"url","token"} as the chat writes its own)
// and logs the launch URL, which it also returns. Every session minted from
// the previous capability ends with it. It refuses to run on a shape where
// the chat holds the capability, so the chat's file is never overwritten.
func (s *Server) RotateOwnerCapability(now time.Time) (string, error) {
	if !s.mint {
		return "", errors.New("the chat holds the owner capability on this shape")
	}
	token := random()
	if err := writeEndpoint(s.Config.OwnerTokenFile, s.Config.Origin, token); err != nil {
		return "", err
	}
	launch := launchURL(s.Config.Origin, token, now)
	s.Logf("Warden launch URL (owner capability; rotates at every edge start; kept in %s): %s", s.Config.OwnerTokenFile, launch)
	return launch, nil
}

// writeEndpoint writes the endpoint file in the chat's shape, privately and
// in one step: a temporary file beside it is renamed into place, so a reader
// (the edge itself, on every request) sees the old capability or the new
// one, never a partial file.
func writeEndpoint(path, origin, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]string{"url": origin, "token": token})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// launchURL is the private launch URL in the shape `warden open` builds
// from the chat's endpoint file: the capability travels in the fragment,
// which the web app stores and strips, and the launch query keeps each
// URL distinct.
func launchURL(origin, token string, now time.Time) string {
	return origin + "/?launch=" + strconv.FormatInt(now.UnixNano(), 10) + "#session=" + token
}
