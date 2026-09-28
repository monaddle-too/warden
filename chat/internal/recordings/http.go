package recordings

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	limits  sync.Once
	uploads chan struct{}
	streams chan struct{}
	Store   *Store
	Objects Objects
	Browser func(*http.Request) (Actor, error)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, e error) {
	status := 503
	message := "Recording service unavailable. Please retry."
	var p *Problem
	if errors.As(e, &p) {
		status = p.Status
		message = p.Message
	}
	if errors.Is(e, ErrDenied) {
		status = 401
		message = e.Error()
	}
	if status == 429 {
		w.Header().Set("Retry-After", "5")
	}
	reply(w, status, map[string]any{"error": message})
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 8193))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return problem(400, "Invalid JSON request.")
	}
	if d.Decode(new(any)) != io.EOF {
		return problem(400, "One JSON object required.")
	}
	return nil
}
func (s *Service) device(r *http.Request) (Actor, string, error) {
	if r.Header.Get("Origin") != "" {
		return Actor{}, "", problem(403, "Device endpoints require a non-browser client.")
	}
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.Store.Authenticate(r.Context(), raw)
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.limits.Do(func() { s.uploads = make(chan struct{}, 2); s.streams = make(chan struct{}, 64) })
	if s.documentation(w, r) {
		return
	}
	browser := strings.HasPrefix(r.URL.Path, "/api/recordings") || strings.HasPrefix(r.URL.Path, "/api/devices")
	var a Actor
	var device string
	var e error
	if browser {
		a, e = s.Browser(r)
	} else {
		a, device, e = s.device(r)
	}
	if e != nil {
		failure(w, e)
		return
	}
	path := r.URL.Path
	if browser && strings.HasPrefix(path, "/api/devices") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/devices"), "/")
		if path == "/api/devices" && r.Method == "GET" {
			v, e := s.Store.Devices(r.Context(), a)
			if e != nil {
				failure(w, e)
			} else {
				reply(w, 200, map[string]any{"items": v})
			}
			return
		}
		if path == "/api/devices" && r.Method == "POST" {
			var in struct {
				Name string `json:"name"`
			}
			if e = decode(r, &in); e == nil {
				var id, key string
				id, key, e = s.Store.Register(r.Context(), a, in.Name)
				if e == nil {
					reply(w, 201, map[string]string{"id": id, "secret": key})
					return
				}
			}
			failure(w, e)
			return
		}
		if len(parts) == 3 && r.Method == "POST" && (parts[2] == "rotate" || parts[2] == "revoke") {
			key, e := s.Store.Manage(r.Context(), a, parts[1], parts[2] == "revoke")
			if e != nil {
				failure(w, e)
			} else {
				reply(w, 200, map[string]string{"secret": key})
			}
			return
		}
		failure(w, problem(404, "Device endpoint not found."))
		return
	}
	base := "/v1/recordings"
	if browser {
		base = "/api/recordings"
	}
	tail := strings.TrimPrefix(path, base)
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if tail == "" {
		if browser && r.Method == "GET" {
			v, e := s.Store.List(r.Context(), a)
			if e != nil {
				failure(w, e)
			} else {
				reply(w, 200, map[string]any{"items": v})
			}
			return
		}
		if !browser && r.Method == "POST" {
			var in Create
			if e = decode(r, &in); e == nil {
				var id string
				id, e = s.Store.Create(r.Context(), a, device, in)
				if e == nil {
					v, e := s.Store.Get(r.Context(), a, device, id)
					if e == nil {
						reply(w, 201, v)
						return
					}
				}
			}
			failure(w, e)
			return
		}
	}
	if browser && tail == "/events" && r.Method == "GET" {
		s.events(w, r, a)
		return
	}
	id := parts[0]
	if len(id) != 32 {
		failure(w, problem(404, "Recording not found."))
		return
	}
	record, e := s.Store.Get(r.Context(), a, device, id)
	if e != nil {
		failure(w, e)
		return
	}
	if len(parts) == 1 && r.Method == "GET" {
		reply(w, 200, record)
		return
	}
	if len(parts) == 2 && parts[1] == "stream" && !browser && r.Method == "GET" {
		if record.Mode != "stream" {
			failure(w, problem(409, "Use a stream recording."))
			return
		}
		s.stream(w, r, a, device, id)
		return
	}
	if len(parts) == 2 && parts[1] == "audio" && browser && r.Method == "GET" {
		s.audio(w, r, record)
		return
	}
	if len(parts) == 2 && parts[1] == "finish" && r.Method == "POST" {
		var in struct {
			NextSeq int64 `json:"nextSeq"`
		}
		if e = decode(r, &in); e == nil {
			e = s.Store.Finish(r.Context(), a, device, id, in.NextSeq)
		}
		if e != nil {
			failure(w, e)
		} else {
			reply(w, 200, map[string]string{"status": "finished"})
		}
		return
	}
	if len(parts) == 2 && parts[1] == "retry" && browser && r.Method == "POST" {
		_, e = s.Store.DB.ExecContext(r.Context(), `UPDATE warden_cloud.audio_segments SET state='waiting',attempts=0 WHERE recording_id=$1 AND state='failed'`, id)
		if e != nil {
			failure(w, e)
		} else {
			reply(w, 200, map[string]string{"status": "queued"})
		}
		return
	}
	if len(parts) == 2 && parts[1] == "upload" && !browser && r.Method == "PUT" {
		if record.Mode != "upload" {
			failure(w, problem(409, "Use an upload recording."))
			return
		}
		s.upload(w, r, a, device, id)
		return
	}
	if len(parts) == 3 && parts[1] == "chunks" && !browser && r.Method == "PUT" {
		seq, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			failure(w, problem(400, "Invalid sequence."))
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, MaxChunk+1))
		if err != nil {
			failure(w, err)
			return
		}
		next, err := s.Store.Append(r.Context(), a, device, id, seq, b)
		if err != nil {
			failure(w, err)
		} else {
			reply(w, 200, map[string]any{"nextSeq": next})
		}
		return
	}
	failure(w, problem(404, "Recording endpoint not found."))
}
func (s *Service) events(w http.ResponseWriter, r *http.Request, a Actor) {
	f, ok := w.(http.Flusher)
	if !ok {
		failure(w, problem(503, "Streaming unavailable."))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	last := ""
	for {
		current, e := s.Browser(r)
		if e != nil || current.OrganizationID != a.OrganizationID {
			return
		}
		var value any
		if id := r.URL.Query().Get("id"); id != "" {
			value, e = s.Store.Get(r.Context(), a, "", id)
		} else {
			var items []Recording
			items, e = s.Store.List(r.Context(), a)
			value = map[string]any{"items": items}
		}
		if e != nil {
			return
		}
		b, _ := json.Marshal(value)
		digest := hash(b)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		if digest != last {
			fmt.Fprintf(w, "id: %s\ndata: %s\n\n", digest, b)
			last = digest
		} else {
			fmt.Fprint(w, ": keepalive\n\n")
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) stream(w http.ResponseWriter, r *http.Request, a Actor, device, id string) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		failure(w, problem(429, "Streaming capacity reached. Retry shortly."))
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }, ReadBufferSize: 4096, WriteBufferSize: 4096}
	c, e := up.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	defer c.Close()
	c.SetReadLimit(MaxChunk + 8)
	type incoming struct {
		kind int
		b    []byte
		e    error
	}
	messages := make(chan incoming, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			_ = c.SetReadDeadline(time.Now().Add(45 * time.Second))
			kind, b, e := c.ReadMessage()
			select {
			case messages <- incoming{kind, b, e}:
			case <-done:
				return
			}
			if e != nil {
				return
			}
		}
	}()
	send := func(v any) error { _ = c.SetWriteDeadline(time.Now().Add(10 * time.Second)); return c.WriteJSON(v) }
	snapshot := func() error {
		_, _, e := s.device(r)
		if e != nil {
			return e
		}
		v, e := s.Store.Get(r.Context(), a, device, id)
		if e != nil {
			return e
		}
		return send(map[string]any{"type": "status", "recording": v})
	}
	if snapshot() != nil {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if snapshot() != nil {
				return
			}
		case m := <-messages:
			if m.e != nil {
				return
			}
			if _, _, e = s.device(r); e != nil {
				_ = send(map[string]any{"type": "error", "status": 401, "error": e.Error()})
				return
			}
			if m.kind == websocket.BinaryMessage && len(m.b) > 8 {
				seq := int64(binary.BigEndian.Uint64(m.b[:8]))
				var next int64
				next, e = s.Store.Append(r.Context(), a, device, id, seq, m.b[8:])
				if e == nil {
					if send(map[string]any{"type": "ack", "nextSeq": next}) != nil {
						return
					}
					continue
				}
			}
			if m.kind == websocket.TextMessage {
				var v struct {
					Type    string `json:"type"`
					NextSeq int64  `json:"nextSeq"`
				}
				if json.Unmarshal(m.b, &v) == nil && v.Type == "finish" {
					e = s.Store.Finish(r.Context(), a, device, id, v.NextSeq)
					if e == nil {
						_ = snapshot()
						return
					}
				} else {
					e = problem(400, "Expected binary audio or a finish message.")
				}
			}
			if e == nil {
				e = problem(400, "Invalid audio message.")
			}
			status := 503
			message := "Recording service unavailable; retry."
			var p *Problem
			if errors.As(e, &p) {
				status = p.Status
				message = p.Message
			}
			if send(map[string]any{"type": "error", "status": status, "error": message}) != nil {
				return
			}
			if status == 401 {
				return
			}
		}
	}
}
func PCMFromWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, problem(400, "Expected a PCM WAV file.")
	}
	var audio []byte
	valid := false
	for pos := 12; pos+8 <= len(b); {
		n := int(binary.LittleEndian.Uint32(b[pos+4:]))
		if n > len(b)-pos-8 {
			return nil, problem(400, "Truncated WAV chunk.")
		}
		body := b[pos+8 : pos+8+n]
		switch string(b[pos : pos+4]) {
		case "fmt ":
			valid = len(body) >= 16 && binary.LittleEndian.Uint16(body) == 1 && binary.LittleEndian.Uint16(body[2:]) == 1 && binary.LittleEndian.Uint32(body[4:]) == 16000 && binary.LittleEndian.Uint16(body[14:]) == 16
		case "data":
			audio = body
		}
		pos += 8 + n + n%2
	}
	if !valid || len(audio) == 0 || len(audio)%2 != 0 {
		return nil, problem(400, "WAV must contain 16 kHz, mono, 16-bit PCM audio.")
	}
	return audio, nil
}
func (s *Service) upload(w http.ResponseWriter, r *http.Request, a Actor, device, id string) {
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		failure(w, problem(429, "Upload capacity reached. Retry shortly."))
		return
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if e != nil {
		failure(w, problem(413, "WAV upload exceeds 32 MiB. Use resumable chunks for longer recordings."))
		return
	}
	audio, e := PCMFromWAV(b)
	if e != nil {
		failure(w, e)
		return
	}
	var seq int64
	for off := 0; off < len(audio); off += MaxChunk {
		end := min(off+MaxChunk, len(audio))
		if _, _, e = s.device(r); e != nil {
			failure(w, e)
			return
		}
		_, e = s.Store.Append(r.Context(), a, device, id, seq, audio[off:end])
		if e != nil {
			failure(w, e)
			return
		}
		seq++
	}
	if e = s.Store.Finish(r.Context(), a, device, id, seq); e != nil {
		failure(w, e)
		return
	}
	v, e := s.Store.Get(r.Context(), a, device, id)
	if e != nil {
		failure(w, e)
	} else {
		reply(w, 200, v)
	}
}
func WAVHeader(size int64) []byte {
	b := make([]byte, 44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(size+36))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(size))
	return b
}
func (s *Service) audio(w http.ResponseWriter, r *http.Request, v Recording) {
	if v.Status != "finished" || v.ArchivedBytes != v.Bytes {
		failure(w, problem(409, "Playback is available once the recording is finished and audio is saved."))
		return
	}
	size := v.Bytes + 44
	start, end := int64(0), size-1
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
		valid := strings.HasPrefix(rangeHeader, "bytes=") && len(parts) == 2
		if valid {
			var e error
			if parts[0] == "" {
				var n int64
				n, e = strconv.ParseInt(parts[1], 10, 64)
				start = max(0, size-n)
				valid = e == nil && n > 0
			} else {
				start, e = strconv.ParseInt(parts[0], 10, 64)
				valid = e == nil
				if parts[1] != "" {
					end, e = strconv.ParseInt(parts[1], 10, 64)
					valid = valid && e == nil
				}
			}
		}
		if !valid || start < 0 || start >= size || end < start {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(416)
			return
		}
		end = min(end, size-1)
	}
	rows, e := s.Store.DB.QueryContext(r.Context(), `SELECT offset_bytes,size,object_key FROM warden_cloud.audio_segments WHERE recording_id=$1 ORDER BY offset_bytes`, v.ID)
	if e != nil {
		failure(w, e)
		return
	}
	type part struct {
		off  int64
		size int
		key  string
	}
	parts := []part{}
	for rows.Next() {
		var p part
		if e = rows.Scan(&p.off, &p.size, &p.key); e != nil {
			rows.Close()
			failure(w, e)
			return
		}
		parts = append(parts, p)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(206)
	}
	write := func(off int64, b []byte) bool {
		lo, hi := max(start, off), min(end+1, off+int64(len(b)))
		if lo >= hi {
			return true
		}
		_, e := w.Write(b[lo-off : hi-off])
		return e == nil
	}
	if !write(0, WAVHeader(v.Bytes)) {
		return
	}
	for _, p := range parts {
		off := p.off + 44
		if off+int64(p.size) <= start || off > end {
			continue
		}
		a, e := s.Objects.Get(r.Context(), p.key)
		if e != nil {
			return
		}
		if !write(off, a) {
			return
		}
	}
}
