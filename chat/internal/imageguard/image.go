// Package imageguard normalizes untrusted images in a separate constrained process.
package imageguard

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"runtime/debug"
)

const MaxBytes = 4 << 20

func Normalize(raw []byte) ([]byte, error) {
	if len(raw) > 8<<20 {
		return nil, errors.New("image too large")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 4_000_000 {
		return nil, errors.New("only PNG/JPEG up to 4 megapixels are supported")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var out limited
	err = png.Encode(&out, img)
	return out.Bytes(), err
}

type limited struct{ bytes.Buffer }

func (b *limited) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxBytes {
		return 0, errors.New("normalized image exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}
func Main() {
	debug.SetMemoryLimit(96 << 20)
	if err := isolate(); err != nil {
		os.Exit(1)
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (8<<20)+1))
	if err != nil {
		os.Exit(1)
	}
	out, err := Normalize(raw)
	if err != nil {
		os.Exit(1)
	}
	if _, err = os.Stdout.Write(out); err != nil {
		os.Exit(1)
	}
}
