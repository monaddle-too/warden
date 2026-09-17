// Package sandbox implements the private, versioned execution-worker protocol.
// The socket is available only to the backend; no host execution API is exposed
// to browsers or to agents. Every operation names a registered project/session.
package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
	"warden/chat/internal/hoststats"
	"warden/chat/internal/release"
	"warden/chat/internal/transport"
)

// ProtocolVersion is the worker protocol number carried in every request and
// response; it is the release-wide Protocol.
const ProtocolVersion = release.Protocol

var ErrBusy = errors.New("sandbox worker is busy")

// WorkerStatus is one runner's reported state.
type WorkerStatus struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Online     bool              `json:"online"`
	Compatible bool              `json:"compatible"`
	Active     int               `json:"active"`
	Capacity   int               `json:"capacity"`
	Revision   string            `json:"revision,omitempty"`
	Stats      *hoststats.Sample `json:"stats,omitempty"`
}

type Request struct {
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	ChatID       string `json:"chatID,omitempty"`
	SandboxID    string `json:"sandboxID,omitempty"`
	RunID        string `json:"runID,omitempty"`
	PrincipalID  string `json:"principalID,omitempty"`
	CallID       string `json:"callID,omitempty"`
	AttachmentID string `json:"attachmentID,omitempty"`
	Port         int    `json:"port,omitempty"`
	// Namespace, Pod, Container, Tail and Previous select pod logs
	// (cluster.logs).
	Namespace       string   `json:"namespace,omitempty"`
	Pod             string   `json:"pod,omitempty"`
	Container       string   `json:"container,omitempty"`
	Tail            int      `json:"tail,omitempty"`
	Previous        bool     `json:"previous,omitempty"`
	Path            string   `json:"path,omitempty"`
	Title           string   `json:"title,omitempty"`
	NewSession      bool     `json:"newSession,omitempty"`
	BundleSize      int64    `json:"bundleSize,omitempty"`
	RemoteHead      string   `json:"remoteHead,omitempty"`
	PublicationHead string   `json:"publicationHead,omitempty"`
	OpenAIAPIKey    string   `json:"openaiAPIKey,omitempty"`
	SourceSessionID string   `json:"sourceSessionID,omitempty"`
	Version         int      `json:"version"`
	Operation       string   `json:"operation"`
	ProjectID       string   `json:"projectID,omitempty"`
	SessionID       string   `json:"sessionID,omitempty"`
	ThreadID        string   `json:"threadID,omitempty"`
	Repository      string   `json:"repository,omitempty"`
	Token           string   `json:"token,omitempty"`
	Directory       string   `json:"directory,omitempty"`
	Args            []string `json:"args,omitempty"`
	Expected        string   `json:"expected,omitempty"`
	// Resources is the size a fresh workspace is created with (bind-chat,
	// prepare) or resized to (resize); nil leaves the sandbox's own.
	Resources *Resources `json:"resources,omitempty"`
}
type Response struct {
	PublishPlan       *RepositoryPublishPlan `json:"publishPlan,omitempty"`
	Review            *RepositoryReview      `json:"review,omitempty"`
	Available         bool                   `json:"available,omitempty"`
	RunnerID          string                 `json:"runnerID,omitempty"`
	Revision          string                 `json:"revision,omitempty"`
	Workers           []WorkerStatus         `json:"workers,omitempty"`
	Stats             *hoststats.Sample      `json:"stats,omitempty"`
	Usage             *SandboxUsage          `json:"usage,omitempty"`
	Progress          *Progress              `json:"progress,omitempty"`
	Pod               *PodInfo               `json:"pod,omitempty"`
	Cluster           *ClusterStatus         `json:"cluster,omitempty"`
	Logs              *PodLogs               `json:"logs,omitempty"`
	ActiveSessions    int                    `json:"activeSessions"`
	SessionLimit      int                    `json:"sessionLimit"`
	ErrorCode         string                 `json:"errorCode,omitempty"`
	Version           int                    `json:"version,omitempty"`
	Sandbox           *SandboxInfo           `json:"sandbox,omitempty"`
	Limits            *ResourceLimits        `json:"limits,omitempty"`
	Attachment        *PreviewAttachment     `json:"attachment,omitempty"`
	Attachments       []PreviewAttachment    `json:"attachments,omitempty"`
	APIKeyPlaceholder string                 `json:"apiKeyPlaceholder,omitempty"`
	RolloutPath       string                 `json:"rolloutPath,omitempty"`
	Error             string                 `json:"error,omitempty"`
	Directory         string                 `json:"directory,omitempty"`
	Output            string                 `json:"output,omitempty"`
	Base              string                 `json:"base,omitempty"`
	Head              string                 `json:"head,omitempty"`
	Diff              string                 `json:"diff,omitempty"`
	Bundle            []byte                 `json:"bundle,omitempty"`
	Bytes             []byte                 `json:"bytes,omitempty"`
}
type SandboxInfo struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectID"`
	RuntimeName string `json:"runtimeName"`
	Directory   string `json:"directory"`
	State       string `json:"state"`
	Generation  string `json:"generation"`
	// Resources is the sandbox's size as the runner holds it; zero for a
	// sandbox registered before sizes existed, which the runner reads as
	// its default.
	Resources Resources `json:"resources"`
}

type PreviewAttachment struct {
	ID        string `json:"id"`
	ChatID    string `json:"chatID"`
	SandboxID string `json:"sandboxID"`
	Port      int    `json:"port"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	URL       string `json:"url,omitempty"`
	State     string `json:"state"`
}

// Client reaches a runner at Address, a unix:// socket (the sbx shapes) or
// a tls:// host:port (Kubernetes, with TLS naming this service's material;
// the runner admits only warden-chat).
type Client struct {
	Address string
	TLS     *transport.TLS
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	o := transport.DialOptions{TLS: c.TLS}
	if transport.IsTLS(c.Address) {
		o.Timeout = 3 * time.Second
	}
	return transport.Dial(ctx, c.Address, o)
}

type connection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *connection) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *Client) Open(ctx context.Context, r Request) (io.ReadWriteCloser, Response, error) {
	r.Version = ProtocolVersion
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, Response{}, fmt.Errorf("execution worker unavailable: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := 5 * time.Minute
	if r.Operation == "prepare" {
		// Creation may wait for a node to join (the runner's PrepareTimeout).
		deadline = 15 * time.Minute
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))
	if err = json.NewEncoder(conn).Encode(r); err != nil {
		conn.Close()
		return nil, Response{}, err
	}
	reader := bufio.NewReaderSize(conn, 65536)
	line, err := readLine(reader, 96<<20)
	if err != nil {
		conn.Close()
		return nil, Response{}, err
	}
	var response Response
	if len(line) > 96<<20 {
		err = fmt.Errorf("worker response too large")
	} else {
		err = json.Unmarshal(line, &response)
	}
	if err == nil && response.Version != ProtocolVersion {
		err = fmt.Errorf("execution worker protocol mismatch")
	}
	if err == nil && response.Error != "" {
		if response.ErrorCode == "busy" {
			err = fmt.Errorf("%w: %s", ErrBusy, response.Error)
		} else {
			err = fmt.Errorf("%s", response.Error)
		}
	}
	if err != nil {
		conn.Close()
		return nil, response, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &connection{conn, reader}, response, nil
}
func (c *Client) Call(ctx context.Context, r Request) (Response, error) {
	conn, res, err := c.Open(ctx, r)
	if conn != nil {
		conn.Close()
	}
	return res, err
}
