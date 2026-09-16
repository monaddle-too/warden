package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentToolsOverrideNativeProxyInBothCasings(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sbx")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			d := &sbxRuntime{worker: &Worker{Executable: fake}}
			stream, err := d.Stream(context.Background(), "sandbox", "/workspace", BrokerConfig{Provider: provider, ProxyURL: "http://host.docker.internal:19000"})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			raw, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
				if !strings.Contains(string(raw), key+"=http://host.docker.internal:19000\n") {
					t.Fatalf("%s tools would inherit SBX proxy: missing %s", provider, key)
				}
			}
		})
	}
}
