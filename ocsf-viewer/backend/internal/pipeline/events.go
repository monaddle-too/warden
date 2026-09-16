package pipeline

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxBody = 10 << 20
const MaxRecords = 2000
const MaxEvent = 256 << 10

//go:embed catalog.json
var catalogJSON []byte
var classes map[string]struct {
	Category   int      `json:"category"`
	Activities []string `json:"activities"`
}

func init() {
	if err := json.Unmarshal(catalogJSON, &classes); err != nil {
		panic(err)
	}
}

type Event struct {
	ID         string `json:"id"`
	BatchID    string `json:"batch_id"`
	Time       int64  `json:"time"`
	ReceivedAt int64  `json:"received_ms"`
	ClassID    int64  `json:"class_uid"`
	Severity   int64  `json:"severity_id"`
	Source     string `json:"source"`
	Stream     string `json:"stream"`
	Raw        string `json:"raw"`
}
type Rejection struct {
	Record int    `json:"record"`
	Reason string `json:"reason"`
	Raw    []byte `json:"raw_base64,omitempty"`
}
type Batch struct {
	ID          string      `json:"id"`
	Stream      string      `json:"stream"`
	Created     int64       `json:"created_ms"`
	Updated     int64       `json:"updated_ms"`
	State       string      `json:"state"`
	Accepted    int         `json:"accepted"`
	Rejected    int         `json:"rejected"`
	Attempts    int         `json:"attempts"`
	NextAttempt int64       `json:"next_attempt_ms"`
	Error       string      `json:"error,omitempty"`
	Hash        string      `json:"-"`
	Key         string      `json:"-"`
	Events      []Event     `json:"events,omitempty"`
	Rejections  []Rejection `json:"rejections,omitempty"`
}

// Receipt metadata is separate from raw event JSON. Original fields are preserved.
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func number(m map[string]any, k string) (int64, bool) {
	n, ok := m[k].(json.Number)
	if !ok {
		return 0, false
	}
	i, e := n.Int64()
	return i, e == nil
}
func nested(m map[string]any, keys ...string) string {
	var value any = m
	for _, k := range keys {
		obj, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = obj[k]
	}
	result, _ := value.(string)
	return result
}
func validate(raw []byte, batchID, stream string, index int, now int64) (Event, error) {
	var e Event
	if len(raw) > MaxEvent {
		return e, fmt.Errorf("record exceeds 256 KiB")
	}
	if !utf8.Valid(raw) {
		return e, fmt.Errorf("record is not UTF-8")
	}
	if !json.Valid(raw) {
		return e, fmt.Errorf("invalid JSON")
	}
	// Bound nesting before decoding/rendering; security logs are untrusted input.
	// Braces inside JSON strings do not contribute to structural depth.
	depth, quoted, escaped := 0, false, false
	for _, c := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
		} else if c == '{' || c == '[' {
			depth++
			if depth > 64 {
				return e, fmt.Errorf("record exceeds 64 nesting levels")
			}
		} else if c == '}' || c == ']' {
			depth--
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var m map[string]any
	if err := decoder.Decode(&m); err != nil || m == nil {
		return e, fmt.Errorf("expected a JSON object")
	}
	values := map[string]int64{}
	for _, k := range []string{"time", "class_uid", "category_uid", "activity_id", "type_uid", "severity_id"} {
		n, ok := number(m, k)
		if !ok {
			return e, fmt.Errorf("%s must be an integer", k)
		}
		values[k] = n
	}
	if values["time"] < 0 || values["time"] > 32503680000000 {
		return e, fmt.Errorf("time must be Unix milliseconds between 1970 and 3000")
	}
	if values["class_uid"] < 1 || values["class_uid"] > 21474836 || values["category_uid"] < 1 || values["activity_id"] < 0 || values["activity_id"] > 99 {
		return e, fmt.Errorf("invalid classification identifiers")
	}
	if values["type_uid"] != values["class_uid"]*100+values["activity_id"] {
		return e, fmt.Errorf("type_uid must equal class_uid * 100 + activity_id")
	}
	severity := values["severity_id"]
	if severity < 0 || (severity > 6 && severity != 99) {
		return e, fmt.Errorf("invalid severity_id")
	}
	if c, ok := classes[strconv.FormatInt(values["class_uid"], 10)]; ok {
		if int64(c.Category) != values["category_uid"] {
			return e, fmt.Errorf("category_uid conflicts with the OCSF class")
		}
		found := len(c.Activities) == 0
		for _, a := range c.Activities {
			if a == strconv.FormatInt(values["activity_id"], 10) {
				found = true
			}
		}
		if !found {
			return e, fmt.Errorf("activity_id is not in the OCSF class enumeration")
		}
	}
	for _, p := range [][]string{{"metadata", "version"}, {"metadata", "product", "name"}, {"metadata", "product", "vendor_name"}} {
		if strings.TrimSpace(nested(m, p...)) == "" {
			return e, fmt.Errorf("%s is required", strings.Join(p, "."))
		}
	}
	e = Event{ID: digest([]byte(fmt.Sprintf("%s:%d", batchID, index))), BatchID: batchID, Time: values["time"], ReceivedAt: now, ClassID: values["class_uid"], Severity: severity, Source: nested(m, "metadata", "product", "name"), Stream: stream, Raw: string(raw)}
	return e, nil
}
func parse(body []byte, contentType, id, stream string, now time.Time) (*Batch, error) {
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	var records []json.RawMessage
	if contentType == "application/x-ndjson" || contentType == "application/ndjson" {
		if bytes.Count(body, []byte{'\n'}) > 10000 {
			return nil, fmt.Errorf("maximum 10000 physical lines per batch")
		}
		// Preserve physical line numbers, including blank lines.
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			records = append(records, bytes.TrimSpace(line))
		}
	} else {
		data := bytes.TrimSpace(body)
		if len(data) == 0 {
			return nil, fmt.Errorf("empty request")
		}
		if data[0] == '[' {
			if err := json.Unmarshal(data, &records); err != nil {
				return nil, fmt.Errorf("invalid JSON array")
			}
		} else {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(data, &envelope); err != nil || envelope == nil {
				return nil, fmt.Errorf("expected an event, array, or {events: [...]} envelope")
			}
			if events, ok := envelope["events"]; ok {
				if len(events) == 0 || events[0] != '[' || json.Unmarshal(events, &records) != nil {
					return nil, fmt.Errorf("events must be an array")
				}
			} else {
				records = []json.RawMessage{data}
			}
		}
	}
	b := &Batch{ID: id, Stream: stream, Created: now.UnixMilli(), Updated: now.UnixMilli(), State: "queued"}
	count := 0
	for index, raw := range records {
		if len(raw) == 0 {
			continue
		}
		count++
		if count > MaxRecords {
			return nil, fmt.Errorf("maximum 2000 records per request")
		}
		e, err := validate(raw, id, stream, index, now.UnixMilli())
		if err != nil {
			b.Rejections = append(b.Rejections, Rejection{Record: index + 1, Reason: err.Error(), Raw: bytes.Clone(raw)})
		} else {
			b.Events = append(b.Events, e)
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("no records found")
	}
	b.Accepted = len(b.Events)
	b.Rejected = len(b.Rejections)
	if b.Accepted == 0 {
		b.State = "quarantined"
	}
	return b, nil
}
