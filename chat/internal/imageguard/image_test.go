package imageguard

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestNormalize(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	for _, format := range []string{"png", "jpeg"} {
		var input bytes.Buffer
		if format == "png" {
			png.Encode(&input, img)
		} else {
			jpeg.Encode(&input, img, nil)
		}
		input.WriteString("hostile trailing metadata")
		out, err := Normalize(input.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("hostile")) {
			t.Fatal("metadata survived")
		}
		c, f, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || f != "png" || c.Width != 20 || c.Height != 10 {
			t.Fatal("invalid normalized image")
		}
	}
}
func TestRejectUnsafeInputs(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`<svg onload="alert(1)"/>`), []byte("GIF89a"), make([]byte, (8<<20)+1), []byte("\x89PNG\r\n\x1a\ninvalid")} {
		if _, err := Normalize(raw); err == nil {
			t.Fatal("accepted unsafe input")
		}
	}
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 4097, 1)))
	if _, err := Normalize(b.Bytes()); err == nil {
		t.Fatal("accepted huge dimensions")
	}
}
