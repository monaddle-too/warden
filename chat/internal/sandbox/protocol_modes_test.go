package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientWorkerProtocolModes(t *testing.T) {
	for _, workerLegacy := range []bool{false, true} {
		for _, clientLegacy := range []bool{false, true} {
			name := "managed-worker"
			if workerLegacy {
				name = "legacy-worker"
			}
			if clientLegacy {
				name += "/legacy-client"
			} else {
				name += "/managed-client"
			}
			t.Run(name, func(t *testing.T) {
				dir, err := os.MkdirTemp("/tmp", "ws-proto-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dir)
				socket := filepath.Join(dir, "s")
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				worker := NewWorker(dir, "must-not-execute", "")
				done := make(chan struct{})
				go func() {
					defer close(done)
					c, err := listener.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					if workerLegacy {
						worker.handleLegacy(context.Background(), c)
					} else {
						worker.handle(context.Background(), c)
					}
				}()
				client := &Client{Socket: socket, Legacy: clientLegacy}
				response, err := client.Call(context.Background(), Request{Operation: "health"})
				<-done
				if workerLegacy == clientLegacy && err != nil {
					t.Fatal(err)
				}
				if workerLegacy != clientLegacy && err == nil {
					t.Fatalf("cross-protocol request accepted: %+v", response)
				}
			})
		}
	}
}

func TestLegacyClientsRejectManagedRequestsBeforeDial(t *testing.T) {
	for _, client := range []*Client{{Legacy: true}, {Pool: true}} {
		for _, request := range []Request{{ChatID: "chat-one"}, {SandboxID: "sandbox-one"}, {PrincipalID: "owner"}} {
			_, err := client.Call(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), "protocol 2") {
				t.Fatalf("managed request reached legacy transport: %v", err)
			}
		}
	}
}

func TestLegacyWorkerRejectsManagedIdentityOnV1(t *testing.T) {
	worker := NewWorker(t.TempDir(), "must-not-execute", "")
	server, client := net.Pipe()
	defer client.Close()
	go func() { defer server.Close(); worker.handleLegacy(context.Background(), server) }()
	if err := json.NewEncoder(client).Encode(Request{Version: 1, Operation: "health", ChatID: "chat-one"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == "" {
		t.Fatal("managed identity accepted on legacy worker")
	}
}
