package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestStreamPreservesRequestsDeltasAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer b.Close()
	received := make(chan Frame, 8)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		scan := bufio.NewScanner(b)
		enc := json.NewEncoder(b)
		for scan.Scan() {
			var f Frame
			_ = json.Unmarshal(scan.Bytes(), &f)
			if f.Method == "initialize" {
				_ = enc.Encode(Frame{ID: f.ID, Result: json.RawMessage(`{}`)})
			}
			if f.Method == "turn/start" {
				_ = enc.Encode(Frame{ID: f.ID, Result: json.RawMessage(`{"turn":{"id":"turn-one"}}`)})
				_ = enc.Encode(Frame{Method: "item/agentMessage/delta", Params: map[string]any{"delta": "streaming"}})
				_ = enc.Encode(Frame{ID: json.RawMessage(`90`), Method: "item/commandExecution/requestApproval"})
			}
			if string(f.ID) == "90" && len(f.Result) > 0 {
				received <- f
			}
		}
	}()
	client, err := StartStream(ctx, a, func(_ *Client, f Frame) { received <- f })
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Call(ctx, "turn/start", map[string]any{})
	if err != nil || String(Map(result["turn"])["id"]) != "turn-one" {
		t.Fatal(err, result)
	}
	delta := <-received
	if delta.Method != "item/agentMessage/delta" {
		t.Fatal(delta)
	}
	approval := <-received
	if err = client.Reply(approval.ID, map[string]string{"decision": "accept"}); err != nil {
		t.Fatal(err)
	}
	reply := <-received
	if string(reply.ID) != "90" {
		t.Fatal(reply)
	}
	client.Close()
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("closing stream did not disconnect worker")
	}
}
