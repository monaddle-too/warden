package recordings

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed docs/*
var documents embed.FS

func Route(path string) bool {
	return path == "/api/devices" || strings.HasPrefix(path, "/api/devices/") || path == "/api/recordings" || strings.HasPrefix(path, "/api/recordings/") || path == "/v1/recordings" || strings.HasPrefix(path, "/v1/recordings/") || strings.HasPrefix(path, "/recording-api/")
}
func (s *Service) documentation(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/recording-api/") {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, "/recording-api/")
	if name == "" {
		name = "index.html"
	}
	b, e := documents.ReadFile("docs/" + name)
	if e != nil {
		http.NotFound(w, r)
		return true
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		return true
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".json"):
		w.Header().Set("Content-Type", "application/json")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Write(b)
	return true
}
