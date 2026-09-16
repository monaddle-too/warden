package policy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"testing"
)

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func zlibBytes(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func mustStream(t *testing.T, secrets []string, encoding string, limit int) *ResponseStream {
	t.Helper()
	s, err := NewResponseStream(secrets, encoding, limit)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStreamEverySecretSplitAndSingleByteChunks(t *testing.T) {
	secret := []byte("synthetic-protected-credential")
	for split := 1; split < len(secret); split++ {
		s := mustStream(t, []string{string(secret)}, "identity", 0)
		out, err := s.Feed(append([]byte("data: "), secret[:split]...))
		if err != nil || string(out) != "data: " {
			t.Fatalf("split %d: %q %v", split, out, err)
		}
		if _, err = s.Feed(secret[split:]); err == nil {
			t.Fatalf("split %d not rejected", split)
		}
	}
	s := mustStream(t, []string{string(secret)}, "identity", 0)
	for _, b := range secret[:len(secret)-1] {
		if out, _ := s.Feed([]byte{b}); len(out) != 0 {
			t.Fatal("prefix delivered")
		}
	}
	if _, err := s.Feed(secret[len(secret)-1:]); err == nil {
		t.Fatal("final byte accepted")
	}
}

func TestStreamSafeEventsPassAndAmbiguousSuffixFlushesAtEOF(t *testing.T) {
	s := mustStream(t, []string{"synthetic-protected-credential"}, "identity", 0)
	if out, _ := s.Feed([]byte("data: hello\n\n")); string(out) != "data: hello\n\n" {
		t.Fatal("event delayed")
	}
	if out, _ := s.Feed([]byte("synthe")); len(out) != 0 {
		t.Fatal("ambiguous suffix delivered")
	}
	if out, _ := s.Feed(nil); string(out) != "synthe" {
		t.Fatal("suffix not flushed at EOF")
	}
	if out, _ := s.Feed(nil); len(out) != 0 || s.Delivered != s.Decoded {
		t.Fatal("post-EOF")
	}
}

func TestStreamOverlappingPrefixesAndMismatch(t *testing.T) {
	s := mustStream(t, []string{"ababac", "ababaX"}, "identity", 0)
	if out, _ := s.Feed([]byte("zzababa")); string(out) != "zz" {
		t.Fatalf("got %q", out)
	}
	if out, _ := s.Feed([]byte("baz")); string(out) != "abababaz" {
		t.Fatalf("got %q", out)
	}
	if out, _ := s.Feed(nil); len(out) != 0 {
		t.Fatal("eof output")
	}
}

func TestStreamWireAndDecodedLimits(t *testing.T) {
	for _, c := range []struct {
		encoding string
		payload  []byte
	}{{"identity", bytes.Repeat([]byte("x"), 33)}, {"gzip", gzipBytes(bytes.Repeat([]byte("x"), 1000))}} {
		s := mustStream(t, nil, c.encoding, 32)
		if _, err := s.Feed(c.payload); err == nil {
			t.Fatalf("%s limit not enforced", c.encoding)
		}
	}
	s := mustStream(t, nil, "identity", 3)
	if out, err := s.Feed([]byte("abc")); err != nil || string(out) != "abc" {
		t.Fatal("within limit")
	}
	if _, err := s.Feed([]byte("d")); err == nil {
		t.Fatal("over limit accepted")
	}
}

func TestStreamCompressedSecretTruncatedAndExtraData(t *testing.T) {
	for _, c := range []struct {
		encoding string
		compress func([]byte) []byte
	}{{"gzip", gzipBytes}, {"deflate", zlibBytes}} {
		s := mustStream(t, []string{"protected-secret"}, c.encoding, 0)
		if _, err := s.Feed(c.compress([]byte("protected-secret"))); err == nil {
			t.Fatalf("%s compressed secret accepted", c.encoding)
		}
		s = mustStream(t, nil, c.encoding, 0)
		compressed := c.compress([]byte("hello"))
		s.Feed(compressed[:len(compressed)-2])
		if _, err := s.Feed(nil); err == nil {
			t.Fatalf("%s truncated stream accepted", c.encoding)
		}
		s = mustStream(t, nil, c.encoding, 0)
		if _, err := s.Feed(append(c.compress([]byte("hello")), []byte("extra")...)); err == nil {
			t.Fatalf("%s trailing data accepted", c.encoding)
		}
	}
}

func TestStreamCompressedOneWireByteAtATime(t *testing.T) {
	body := bytes.Repeat([]byte("data: snowman \xe2\x98\x83\n\n"), 10)
	for _, c := range []struct {
		encoding string
		compress func([]byte) []byte
	}{{"gzip", gzipBytes}, {"deflate", zlibBytes}} {
		s := mustStream(t, []string{"unrelated-secret"}, c.encoding, 0)
		var output []byte
		for _, b := range c.compress(body) {
			out, err := s.Feed([]byte{b})
			if err != nil {
				t.Fatalf("%s byte feed: %v", c.encoding, err)
			}
			output = append(output, out...)
		}
		out, err := s.Feed(nil)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, out...)
		if !bytes.Equal(output, body) {
			t.Fatalf("%s output mismatch", c.encoding)
		}
	}
}

func TestStreamUnsupportedEncodingRejected(t *testing.T) {
	if _, err := NewResponseStream(nil, "br", 0); err == nil {
		t.Fatal("br accepted")
	}
}
