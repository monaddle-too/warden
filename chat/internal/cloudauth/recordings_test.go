package cloudauth

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"warden/chat/internal/recordings"
)

type audioMemory struct {
	mu   sync.Mutex
	data map[string][]byte
	fail bool
}

func (m *audioMemory) Put(_ context.Context, k string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = append([]byte(nil), b...)
	return nil
}
func (m *audioMemory) Get(_ context.Context, k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data[k]...), nil
}
func (m *audioMemory) Transcribe(_ context.Context, b []byte, _ string) (string, error) {
	if m.fail {
		return "", errors.New("provider unavailable")
	}
	return fmt.Sprintf("Recognized %d samples", len(b)/2), nil
}
func TestDeviceRecordingsPostgres(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	store, e := Open(ctx, dsn, "owner@example.com")
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	owner, e := store.UserByEmail(ctx, "owner@example.com")
	if e != nil {
		t.Fatal(e)
	}
	org, e := store.CreateOrganization(ctx, owner, "Audio "+ID())
	if e != nil {
		t.Fatal(e)
	}
	member, e := store.PutMember(ctx, owner, org.ID, ID()+"@example.com", "Recorder friend", "user")
	if e != nil {
		t.Fatal(e)
	}
	session, cookie, e := store.newPasskeySession(ctx, member.User)
	if e != nil {
		t.Fatal(e)
	}
	auth := &Auth{Login: &Login{Store: store}}
	audioStore := &recordings.Store{DB: store.db}
	mem := &audioMemory{data: map[string][]byte{}}
	service := &recordings.Service{Store: audioStore, Objects: mem, Browser: auth.RecordingActor}
	server := httptest.NewServer(service)
	defer server.Close()
	auth.Origin = server.URL
	call := func(method, path, key string, body any, want int) []byte {
		t.Helper()
		var raw []byte
		switch v := body.(type) {
		case []byte:
			raw = v
		case nil:
		default:
			raw, _ = json.Marshal(v)
		}
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			req.Header.Set("Origin", server.URL)
			req.Header.Set("X-Warden-CSRF", session.CSRF)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		return b
	}
	var credential struct{ ID, Secret string }
	json.Unmarshal(call("POST", "/api/devices", "", map[string]string{"name": "Test board"}, 201), &credential)
	if !strings.HasPrefix(credential.Secret, "wrd_") {
		t.Fatal("missing key")
	}
	if strings.Contains(string(call("GET", "/api/devices", "", nil, 200)), credential.Secret) {
		t.Fatal("secret leaked")
	}
	in := recordings.Create{OperationID: ID(), Title: "Live test", Language: "en-US", Mode: "stream", Encoding: "pcm_s16le", SampleRate: 16000, Channels: 1}
	var rec recordings.Recording
	json.Unmarshal(call("POST", "/v1/recordings", credential.Secret, in, 201), &rec)
	id := rec.ID
	var again recordings.Recording
	json.Unmarshal(call("POST", "/v1/recordings", credential.Secret, in, 201), &again)
	if again.ID != id {
		t.Fatal("create not idempotent")
	}
	bad := in
	bad.Title = "different"
	call("POST", "/v1/recordings", credential.Secret, bad, 409)
	actor := recordings.Actor{UserID: member.ID, OrganizationID: org.ID}
	foreign := recordings.Actor{OrganizationID: ID()}
	if _, e = audioStore.Get(ctx, foreign, "", id); e == nil {
		t.Fatal("cross-org read")
	}
	connect := func() *websocket.Conn {
		t.Helper()
		header := http.Header{"Authorization": []string{"Bearer " + credential.Secret}}
		ws, _, e := websocket.DefaultDialer.Dial(strings.Replace(server.URL, "http", "ws", 1)+"/v1/recordings/"+id+"/stream", header)
		if e != nil {
			t.Fatal(e)
		}
		var hello map[string]any
		if e = ws.ReadJSON(&hello); e != nil {
			t.Fatal(e)
		}
		if hello["type"] != "status" {
			t.Fatal(hello)
		}
		return ws
	}
	ws := connect()
	chunk := make([]byte, 32000)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	send := func(seq uint64) {
		t.Helper()
		frame := make([]byte, 8+len(chunk))
		binary.BigEndian.PutUint64(frame, seq)
		copy(frame[8:], chunk)
		if e = ws.WriteMessage(websocket.BinaryMessage, frame); e != nil {
			t.Fatal(e)
		}
		for {
			var ack struct {
				Type    string
				NextSeq uint64
			}
			if e = ws.ReadJSON(&ack); e != nil {
				t.Fatal(e)
			}
			if ack.Type == "ack" {
				if ack.NextSeq != seq+1 {
					t.Fatal(ack)
				}
				break
			}
			if ack.Type == "error" {
				t.Fatal("stream error")
			}
		}
	}
	for i := uint64(0); i < 5; i++ {
		send(i)
	}
	ws.Close()
	ws = connect()
	var state recordings.Recording
	json.Unmarshal(call("GET", "/v1/recordings/"+id, credential.Secret, nil, 200), &state)
	if state.Bytes != 160000 || state.NextSeq != 5 {
		t.Fatal(state)
	}
	call("PUT", "/v1/recordings/"+id+"/chunks/4", credential.Secret, chunk, 200)
	different := append([]byte(nil), chunk...)
	different[0] = 99
	call("PUT", "/v1/recordings/"+id+"/chunks/4", credential.Secret, different, 409)
	call("PUT", "/v1/recordings/"+id+"/chunks/7", credential.Secret, chunk, 409)
	worker := &recordings.Worker{Store: audioStore, Objects: mem, Transcriber: mem}
	if _, e = worker.Step(ctx); e != nil {
		t.Fatal(e)
	}
	state, _ = audioStore.Get(ctx, actor, "", id)
	if state.Provisional == "" {
		t.Fatal("no provisional transcript")
	}
	for i := uint64(5); i < 12; i++ {
		send(i)
	}
	ws.Close()
	call("POST", "/v1/recordings/"+id+"/finish", credential.Secret, map[string]int{"nextSeq": 11}, 409)
	call("POST", "/v1/recordings/"+id+"/finish", credential.Secret, map[string]int{"nextSeq": 12}, 200)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := worker.Step(ctx)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	for range 3 {
		if _, e = worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
	}
	state, _ = audioStore.Get(ctx, actor, "", id)
	if state.Transcription != "complete" || len(state.Segments) != 2 || state.ArchivedBytes != 384000 {
		t.Fatalf("not complete: %+v", state)
	}
	wav := call("GET", "/api/recordings/"+id+"/audio", "", nil, 200)
	pcm, e := recordings.PCMFromWAV(wav)
	if e != nil || len(pcm) != 384000 {
		t.Fatalf("audio %d %v", len(pcm), e)
	}
	if !bytes.Equal(pcm[:32000], chunk) {
		t.Fatal("audio corrupt")
	}
	// Restart recovery: acknowledged bytes remain in the database, expired leases resume.
	in.OperationID = ID()
	in.Title = "Upload"
	in.Mode = "upload"
	json.Unmarshal(call("POST", "/v1/recordings", credential.Secret, in, 201), &rec)
	small := append(recordings.WAVHeader(32000), chunk...)
	call("PUT", "/v1/recordings/"+rec.ID+"/upload", credential.Secret, small, 200)
	call("PUT", "/v1/recordings/"+rec.ID+"/upload", credential.Secret, small, 200)
	store.db.Exec(`UPDATE warden_cloud.recordings SET lease_until=now()-interval '1 second',lease_token='dead' WHERE id=$1`, rec.ID)
	mem.fail = true
	for range 3 {
		if _, e = worker.Step(ctx); e == nil {
			t.Fatal("expected transcription failure")
		}
		store.db.Exec(`UPDATE warden_cloud.recordings SET retry_at=NULL WHERE id=$1`, rec.ID)
	}
	state, _ = audioStore.Get(ctx, actor, "", rec.ID)
	if state.Transcription != "failed" || state.ArchivedBytes != 32000 {
		t.Fatal(state)
	}
	call("GET", "/api/recordings/"+rec.ID+"/audio", "", nil, 200)
	mem.fail = false
	call("POST", "/api/recordings/"+rec.ID+"/retry", "", map[string]any{}, 200)
	if _, e = worker.Step(ctx); e != nil {
		t.Fatal(e)
	}
	call("POST", "/api/devices/"+credential.ID+"/rotate", "", map[string]any{}, 200)
	call("GET", "/v1/recordings/"+id, credential.Secret, nil, 401)
	// Removing the registering member invalidates the new secret too.
	var rotated struct{ Secret string }
	json.Unmarshal(call("POST", "/api/devices/"+credential.ID+"/rotate", "", map[string]any{}, 200), &rotated)
	store.db.Exec(`DELETE FROM warden_cloud.memberships WHERE user_id=$1 AND organization_id=$2`, member.ID, org.ID)
	call("GET", "/v1/recordings/"+id, rotated.Secret, nil, 401)
	// No upload access with a device bearer on browser management endpoints.
	call("GET", "/api/devices", credential.Secret, nil, 401)
}
