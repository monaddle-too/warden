package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Receipt is the receiver's answer: the report's id (the client's when it
// was well formed) and when it was received.
type Receipt struct {
	ID       string `json:"id"`
	Received string `json:"received"`
}

// SendError is a receiver's refusal, with what to do about it.
type SendError struct {
	Status     int
	RetryAfter string
	Body       string
	URL        string
}

func (e *SendError) Error() string {
	switch e.Status {
	case http.StatusNotFound:
		return fmt.Sprintf("the receiver at %s is not accepting bug reports (404); reporting may be disabled there", e.URL)
	case http.StatusRequestEntityTooLarge:
		return "the report is larger than the receiver accepts (413); shorten the description or report without the logs"
	case http.StatusTooManyRequests:
		if e.RetryAfter != "" {
			return "the receiver is rate-limiting reports (429); try again after " + e.RetryAfter
		}
		return "the receiver is rate-limiting reports (429); try again later"
	case http.StatusBadRequest:
		return "the receiver rejected the report (400): " + Clip(strings.TrimSpace(e.Body), 200)
	}
	return fmt.Sprintf("the receiver answered %d: %s", e.Status, Clip(strings.TrimSpace(e.Body), 200))
}

// HTTPClient sends reports; tests may replace it.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// Send posts the report to url and returns the receipt. A refusal is a
// *SendError naming the status; a report over MaxBody is refused before
// it is sent; a network failure says so.
func Send(ctx context.Context, url string, r Report) (Receipt, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Receipt{}, err
	}
	if len(body) > MaxBody {
		return Receipt{}, fmt.Errorf("the report is %d KiB, over the receiver's %d KiB limit; shorten the description or report without the logs", len(body)>>10, MaxBody>>10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := HTTPClient.Do(req)
	if err != nil {
		return Receipt{}, fmt.Errorf("could not reach the receiver: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusAccepted && res.StatusCode != http.StatusOK {
		return Receipt{}, &SendError{Status: res.StatusCode, RetryAfter: res.Header.Get("Retry-After"), Body: string(data), URL: url}
	}
	var receipt Receipt
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.ID == "" {
		return Receipt{}, errors.New("the receiver accepted the report but answered without an id")
	}
	return receipt, nil
}
