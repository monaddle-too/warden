package cloudauth

import (
	"context"
	"net/http"
	"os"
	"time"
	"warden/chat/internal/recordings"
)

func (a *Auth) StartRecordings() {
	bucket, project := os.Getenv("WARDEN_RECORDINGS_BUCKET"), os.Getenv("WARDEN_RECORDINGS_PROJECT")
	if bucket == "" || project == "" {
		return
	}
	store := &recordings.Store{DB: a.Login.Store.db}
	cloud := &recordings.Google{Bucket: bucket, Project: project, Client: &http.Client{Timeout: 40 * time.Second}}
	a.Recordings = &recordings.Service{Store: store, Objects: cloud, Browser: a.RecordingActor}
	ctx, cancel := context.WithCancel(context.Background())
	a.Login.Store.recordingCancel = cancel
	worker := &recordings.Worker{Store: store, Objects: cloud, Transcriber: cloud}
	go worker.Run(ctx)
}
func (a *Auth) RecordingActor(r *http.Request) (recordings.Actor, error) {
	s, e := a.current(r)
	if e != nil {
		return recordings.Actor{}, &recordings.Problem{Status: 401, Message: "Sign in to access recordings."}
	}
	if s.OrganizationID == "" {
		return recordings.Actor{}, &recordings.Problem{Status: 403, Message: "Select an organization."}
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != a.Origin {
		return recordings.Actor{}, &recordings.Problem{Status: 403, Message: "Origin refused."}
	}
	if r.Method != "GET" && r.Method != "HEAD" && !a.csrf(r, s) {
		return recordings.Actor{}, &recordings.Problem{Status: 403, Message: "Refresh the page and retry."}
	}
	return recordings.Actor{UserID: s.User.ID, OrganizationID: s.OrganizationID, Admin: s.User.FullAdmin || s.Role == "admin"}, nil
}
func (a *Auth) RecordingRoute(w http.ResponseWriter, r *http.Request) bool {
	if !recordings.Route(r.URL.Path) {
		return false
	}
	if a.Recordings == nil {
		respond(w, 503, map[string]string{"error": "Recordings are not configured on this installation."})
		return true
	}
	a.Recordings.ServeHTTP(w, r)
	return true
}
