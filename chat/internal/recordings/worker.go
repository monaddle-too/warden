package recordings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Objects interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
}
type Transcriber interface {
	Transcribe(context.Context, []byte, string) (string, error)
}
type Worker struct {
	Store       *Store
	Objects     Objects
	Transcriber Transcriber
}

// Run can execute on overlapping replicas. A fenced, expiring PostgreSQL lease
// owns each recording; object writes are deterministic and safe to repeat.
func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for i := 0; i < 4; i++ {
				worked, err := w.Step(ctx)
				if err != nil && ctx.Err() == nil {
					log.Printf("recording worker: %v", err)
				}
				if !worked {
					break
				}
			}
		}
	}
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	token := ID()
	var id, org, language, status string
	var total, archived, provisional int64
	var updated time.Time
	e := w.Store.DB.QueryRowContext(ctx, `UPDATE warden_cloud.recordings SET lease_token=$1,lease_until=now()+interval '90 seconds' WHERE id=(SELECT r.id FROM warden_cloud.recordings r WHERE (lease_until IS NULL OR lease_until<now()) AND (retry_at IS NULL OR retry_at<now()) AND (bytes>archived_bytes AND (bytes-archived_bytes>=320000 OR status='finished' OR updated_at<now()-interval '15 seconds' OR bytes-archived_bytes>=160000 AND provisional_bytes=0) OR EXISTS(SELECT 1 FROM warden_cloud.audio_segments s WHERE s.recording_id=r.id AND s.state='waiting')) ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id,organization_id,language,status,bytes,archived_bytes,provisional_bytes,updated_at`, token).Scan(&id, &org, &language, &status, &total, &archived, &provisional, &updated)
	if e == sql.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	workCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	e = w.process(workCtx, id, org, language, status, total, archived, provisional, updated, token)
	retry := 0
	message := ""
	if e != nil {
		retry = 10
		message = "Audio processing temporarily unavailable; retrying automatically."
	}
	_, releaseErr := w.Store.DB.ExecContext(ctx, `UPDATE warden_cloud.recordings SET lease_until=NULL,worker_error=$3,retry_at=now()+($4*interval '1 second') WHERE id=$1 AND lease_token=$2`, id, token, message, retry)
	if e != nil {
		return true, e
	}
	return true, releaseErr
}
func (w *Worker) process(ctx context.Context, id, org, language, status string, total, archived, provisional int64, updated time.Time, token string) error {
	if total > archived {
		rows, e := w.Store.DB.QueryContext(ctx, `SELECT audio FROM warden_cloud.audio_chunks WHERE recording_id=$1 AND offset_bytes >= $2 AND offset_bytes < $2+320000 ORDER BY seq`, id, archived)
		if e != nil {
			return e
		}
		audio := []byte{}
		for rows.Next() {
			var b []byte
			if e = rows.Scan(&b); e != nil {
				rows.Close()
				return e
			}
			audio = append(audio, b...)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		final := len(audio) >= 320000 || status == "finished" || time.Since(updated) > 15*time.Second
		if len(audio) > 0 && final {
			key := fmt.Sprintf("%s/%s/%012d.pcm", org, id, archived)
			if e = w.Objects.Put(ctx, key, audio); e != nil {
				return e
			}
			tx, e := w.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			res, e := tx.ExecContext(ctx, `UPDATE warden_cloud.recordings SET archived_bytes=$3,provisional='',provisional_bytes=0 WHERE id=$1 AND lease_token=$2 AND lease_until>now()`, id, token, archived+int64(len(audio)))
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return fmt.Errorf("worker lease expired")
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO warden_cloud.audio_segments(recording_id,offset_bytes,size,object_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, archived, len(audio), key)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `UPDATE warden_cloud.audio_chunks SET audio=NULL WHERE recording_id=$1 AND offset_bytes >= $2 AND offset_bytes < $3`, id, archived, archived+int64(len(audio)))
			if e != nil {
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
		} else if len(audio) >= 160000 && provisional == 0 {
			text, e := w.Transcriber.Transcribe(ctx, audio, language)
			// A failed provisional pass never blocks durable capture or the final pass.
			if e != nil {
				text = ""
			}
			_, e = w.Store.DB.ExecContext(ctx, `UPDATE warden_cloud.recordings SET provisional=$3,provisional_bytes=$4 WHERE id=$1 AND lease_token=$2`, id, token, text, len(audio))
			return e
		}
	}
	var offset int64
	var key string
	var attempts int
	e := w.Store.DB.QueryRowContext(ctx, `SELECT offset_bytes,object_key,attempts FROM warden_cloud.audio_segments WHERE recording_id=$1 AND state='waiting' ORDER BY offset_bytes LIMIT 1`, id).Scan(&offset, &key, &attempts)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	audio, e := w.Objects.Get(ctx, key)
	if e != nil {
		return e
	}
	text, transcribeErr := w.Transcriber.Transcribe(ctx, audio, language)
	state := "complete"
	if transcribeErr != nil {
		state = "waiting"
		if attempts >= 2 {
			state = "failed"
		}
		text = ""
	}
	_, e = w.Store.DB.ExecContext(ctx, `UPDATE warden_cloud.audio_segments SET text=$3,state=$4,attempts=attempts+1 WHERE recording_id=$1 AND offset_bytes=$2 AND EXISTS(SELECT 1 FROM warden_cloud.recordings WHERE id=$1 AND lease_token=$5 AND lease_until>now())`, id, offset, text, state, token)
	if e != nil {
		return e
	}
	if transcribeErr != nil {
		return transcribeErr
	}
	return nil
}

// Google uses the GKE workload identity metadata server, never a device key or
// a long-lived downloaded service-account credential.
type Google struct {
	Bucket, Project string
	Client          *http.Client
	mu              sync.Mutex
	token           string
	expires         time.Time
}

func (g *Google) access(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Until(g.expires) > time.Minute {
		return g.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	res, e := g.Client.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("workload identity unavailable: %d", res.StatusCode)
	}
	var v struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&v); e != nil {
		return "", e
	}
	g.token = v.Token
	g.expires = time.Now().Add(time.Duration(v.Expires) * time.Second)
	return g.token, nil
}
func (g *Google) call(ctx context.Context, method, address, contentType string, data []byte) ([]byte, error) {
	token, e := g.access(ctx)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Goog-User-Project", g.Project)
	res, e := g.Client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if e != nil {
		return nil, e
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("cloud audio service status %d", res.StatusCode)
	}
	return b, nil
}
func (g *Google) Put(ctx context.Context, key string, audio []byte) error {
	_, e := g.call(ctx, "POST", "https://storage.googleapis.com/upload/storage/v1/b/"+url.PathEscape(g.Bucket)+"/o?uploadType=media&name="+url.QueryEscape(key), "application/octet-stream", audio)
	return e
}
func (g *Google) Get(ctx context.Context, key string) ([]byte, error) {
	return g.call(ctx, "GET", "https://storage.googleapis.com/storage/v1/b/"+url.PathEscape(g.Bucket)+"/o/"+url.PathEscape(key)+"?alt=media", "", nil)
}
func (g *Google) Transcribe(ctx context.Context, audio []byte, language string) (string, error) {
	body, _ := json.Marshal(map[string]any{"config": map[string]any{"encoding": "LINEAR16", "sampleRateHertz": 16000, "audioChannelCount": 1, "languageCode": language, "enableAutomaticPunctuation": true}, "audio": map[string]string{"content": base64.StdEncoding.EncodeToString(audio)}})
	b, e := g.call(ctx, "POST", "https://speech.googleapis.com/v1/speech:recognize", "application/json", body)
	if e != nil {
		return "", e
	}
	var v struct {
		Results []struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"results"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return "", e
	}
	out := []string{}
	for _, r := range v.Results {
		if len(r.Alternatives) > 0 {
			out = append(out, r.Alternatives[0].Transcript)
		}
	}
	return strings.Join(out, " "), nil
}
