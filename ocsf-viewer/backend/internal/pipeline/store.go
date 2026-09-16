package pipeline

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"time"
)

var ErrConflict = errors.New("idempotency key was already used for different content")
var ErrFull = errors.New("durable queue or disk is full; retry later")
var ErrMissing = errors.New("batch not found")
var batches = []byte("batches")
var keys = []byte("keys")
var pending = []byte("pending")
var meta = []byte("meta")

type storedBatch struct {
	Batch *Batch `json:"batch"`
	Hash  string `json:"hash"`
	Key   string `json:"key"`
}
type Store struct {
	db        *bolt.DB
	maxBytes  int64
	directory string
}

func OpenStore(path string, maxBytes int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{batches, keys, pending, meta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, maxBytes: maxBytes, directory: filepath.Dir(path)}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func decode(v []byte) (*storedBatch, error) {
	if v == nil {
		return nil, ErrMissing
	}
	var b storedBatch
	err := json.Unmarshal(v, &b)
	return &b, err
}
func value(tx *bolt.Tx, key string) int64 {
	v := tx.Bucket(meta).Get([]byte(key))
	if len(v) != 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(v))
}
func add(tx *bolt.Tx, key string, delta int64) error {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(value(tx, key)+delta))
	return tx.Bucket(meta).Put([]byte(key), v[:])
}
func account(tx *bolt.Tx, b *Batch, size int, sign int64) error {
	for key, n := range map[string]int64{b.State: 1, "accepted": int64(b.Accepted), "rejected": int64(b.Rejected), "bytes": int64(size)} {
		if err := add(tx, key, n*sign); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Get(id string) (*Batch, error) {
	var out *Batch
	err := s.db.View(func(tx *bolt.Tx) error {
		v, e := decode(tx.Bucket(batches).Get([]byte(id)))
		if e == nil {
			out = v.Batch
		}
		return e
	})
	return out, err
}
func (s *Store) Put(b *Batch, key, hash string) (*Batch, bool, error) {
	var out *Batch
	duplicate := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		rows, ids := tx.Bucket(batches), tx.Bucket(keys)
		if id := ids.Get([]byte(key)); id != nil {
			old, e := decode(rows.Get(id))
			if e != nil {
				return e
			}
			if old.Hash != hash {
				return ErrConflict
			}
			out = old.Batch
			duplicate = true
			return nil
		}
		encoded, e := json.Marshal(storedBatch{Batch: b, Hash: hash, Key: key})
		if e != nil {
			return e
		}
		if value(tx, "bytes")+int64(len(encoded)) > s.maxBytes {
			return ErrFull
		}
		var disk unix.Statfs_t
		if err := unix.Statfs(s.directory, &disk); err != nil {
			return err
		}
		if uint64(disk.Bavail)*uint64(disk.Bsize) < 1<<30 {
			return ErrFull
		}
		if e = rows.Put([]byte(b.ID), encoded); e != nil {
			return e
		}
		if e = ids.Put([]byte(key), []byte(b.ID)); e != nil {
			return e
		}
		if b.State == "queued" {
			if e = tx.Bucket(pending).Put([]byte(b.ID), []byte{1}); e != nil {
				return e
			}
		}
		if e = account(tx, b, len(encoded), 1); e != nil {
			return e
		}
		out = b
		return nil
	})
	return out, duplicate, err
}
func (s *Store) Update(id string, fn func(*Batch) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		rows := tx.Bucket(batches)
		old := rows.Get([]byte(id))
		b, err := decode(old)
		if err != nil {
			return err
		}
		if err = account(tx, b.Batch, len(old), -1); err != nil {
			return err
		}
		if err = fn(b.Batch); err != nil {
			return err
		}
		b.Batch.Updated = time.Now().UnixMilli()
		v, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if b.Batch.State == "queued" {
			err = tx.Bucket(pending).Put([]byte(id), []byte{1})
		} else {
			err = tx.Bucket(pending).Delete([]byte(id))
		}
		if err != nil {
			return err
		}
		if err = account(tx, b.Batch, len(v), 1); err != nil {
			return err
		}
		return rows.Put([]byte(id), v)
	})
}
func (s *Store) List(limit int) ([]Batch, error) {
	out := []Batch{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(batches).Cursor()
		for _, v := c.Last(); v != nil && len(out) < limit; _, v = c.Prev() {
			b, e := decode(v)
			if e != nil {
				return e
			}
			b.Batch.Events = nil
			b.Batch.Rejections = nil
			out = append(out, *b.Batch)
		}
		return nil
	})
	return out, err
}
func (s *Store) Next() (*Batch, error) {
	var out *Batch
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(pending).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			b, e := decode(tx.Bucket(batches).Get(k))
			if e != nil {
				return e
			}
			if b.Batch.NextAttempt <= time.Now().UnixMilli() {
				out = b.Batch
				break
			}
		}
		return nil
	})
	return out, err
}
func (s *Store) Stats() (map[string]int64, error) {
	out := map[string]int64{"capacity_bytes": s.maxBytes}
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, key := range []string{"queued", "indexed", "quarantined", "dead_letter", "accepted", "rejected", "bytes"} {
			out[key] = value(tx, key)
		}
		return nil
	})
	return out, err
}
func (s *Store) Cleanup(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		rows := tx.Bucket(batches)
		var expired []string
		err := rows.ForEach(func(k, v []byte) error {
			b, e := decode(v)
			if e != nil {
				return e
			}
			if b.Batch.State != "queued" && b.Batch.State != "dead_letter" && b.Batch.Updated < now.Add(-7*24*time.Hour).UnixMilli() {
				expired = append(expired, string(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, id := range expired {
			old := rows.Get([]byte(id))
			b, _ := decode(old)
			if err := account(tx, b.Batch, len(old), -1); err != nil {
				return err
			}
			if err := tx.Bucket(keys).Delete([]byte(b.Key)); err != nil {
				return err
			}
			if err := rows.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
}
