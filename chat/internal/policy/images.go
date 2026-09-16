package policy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"regexp"
)

var imageIDShape = regexp.MustCompile(`^[a-f0-9]{64}$`)
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// Images stores private immutable PNGs accepted only from the host's image
// normalizer, scoped to one chat and sandbox.
type Images struct{ s *Sharing }

// StoredImage is one image row.
type StoredImage struct {
	ID, Chat, Sandbox, Caption, Digest string
	PNG                                []byte
}

func newImages(s *Sharing) (*Images, error) {
	if _, err := s.DB.Exec("CREATE TABLE IF NOT EXISTS images (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, caption TEXT, digest TEXT, png BLOB)"); err != nil {
		return nil, err
	}
	return &Images{s: s}, nil
}

// Get returns an image owned by the conversation, verifying its digest.
func (i *Images) Get(id, chat, sandbox string) (*StoredImage, error) {
	if !imageIDShape.MatchString(id) {
		return nil, valueErr("invalid image")
	}
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	return i.getLocked(id, chat, sandbox)
}

func (i *Images) getLocked(id, chat, sandbox string) (*StoredImage, error) {
	var img StoredImage
	err := i.s.DB.QueryRow("SELECT id,chat,sandbox,caption,digest,png FROM images WHERE id=? AND chat=? AND sandbox=?", id, chat, sandbox).Scan(&img.ID, &img.Chat, &img.Sandbox, &img.Caption, &img.Digest, &img.PNG)
	if err != nil || sha256Hex(img.PNG) != img.Digest {
		return nil, valueErr("image unavailable for this conversation")
	}
	return &img, nil
}

// Dispatch handles image_get and image_add.
func (i *Images) Dispatch(op string, d map[string]any) (map[string]any, error) {
	chat, sandbox := stringField(d, "chatID"), stringField(d, "sandboxID")
	if !validIdentifier(chat) || !validIdentifier(sandbox) {
		return nil, errors.New("invalid image context")
	}
	if op == "image_get" {
		img, err := i.Get(stringField(d, "id"), chat, sandbox)
		if err != nil {
			return nil, err
		}
		return map[string]any{"png": base64.StdEncoding.EncodeToString(img.PNG)}, nil
	}
	if op != "image_add" {
		return nil, errors.New("invalid image action")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(stringField(d, "png"))
	if err != nil {
		return nil, errors.New("invalid normalized image")
	}
	caption, ok := d["caption"].(string)
	if len(raw) > 4*1024*1024 || !bytes.HasPrefix(raw, pngMagic) || !ok || len(caption) > 500 {
		return nil, errors.New("invalid normalized image")
	}
	digest := sha256Hex(raw)
	id := sha256Hex([]byte(chat + "\x00" + sandbox + "\x00" + digest))
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	var exists int
	if err = i.s.DB.QueryRow("SELECT 1 FROM images WHERE id=?", id).Scan(&exists); err != nil {
		var count, size, total int64
		if err = i.s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(length(png)),0) FROM images WHERE chat=?", chat).Scan(&count, &size); err != nil {
			return nil, err
		}
		if err = i.s.DB.QueryRow("SELECT COALESCE(SUM(length(png)),0) FROM images").Scan(&total); err != nil {
			return nil, err
		}
		if count >= 100 || size+int64(len(raw)) > 64*1024*1024 || total+int64(len(raw)) > 512*1024*1024 {
			return nil, errors.New("image storage quota reached")
		}
		if _, err = i.s.DB.Exec("INSERT INTO images VALUES (?,?,?,?,?,?)", id, chat, sandbox, caption, digest, raw); err != nil {
			return nil, err
		}
	}
	return map[string]any{"image_id": id, "caption": caption, "sha256": digest, "mime_type": "image/png"}, nil
}
