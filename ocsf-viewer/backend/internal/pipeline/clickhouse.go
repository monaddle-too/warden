package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ClickHouse struct {
	URL, User, Password string
	Client              *http.Client
}
type dbError struct {
	Status  int
	Message string
}

func (e *dbError) Error() string { return e.Message }
func (ch *ClickHouse) query(ctx context.Context, sql string, params url.Values, body []byte) ([]byte, error) {
	endpoint, err := url.Parse(ch.URL)
	if err != nil {
		return nil, err
	}
	q := endpoint.Query()
	q.Set("query", sql)
	q.Set("wait_end_of_query", "1")
	q.Set("max_execution_time", "15")
	q.Set("max_memory_usage", "536870912")
	for key, values := range params {
		for _, v := range values {
			q.Add("param_"+key, v)
		}
	}
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(ch.User, ch.Password)
	resp, err := ch.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ClickHouse connection unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &dbError{Status: resp.StatusCode, Message: fmt.Sprintf("ClickHouse HTTP %d", resp.StatusCode)}
	}
	// ClickHouse can encode a late query exception in an HTTP-200 response.
	if resp.Header.Get("X-ClickHouse-Exception-Code") != "" || bytes.HasPrefix(data, []byte("Code: ")) {
		return nil, fmt.Errorf("ClickHouse query failed")
	}
	return data, nil
}
func (ch *ClickHouse) Init(ctx context.Context) error {
	sql := `CREATE TABLE IF NOT EXISTS ocsf.events (
 id String, batch_id String, time Int64, received_ms Int64, class_uid Int64,
 severity_id Int64, source LowCardinality(String), stream LowCardinality(String), raw String,
 received_at DateTime DEFAULT toDateTime(intDiv(received_ms,1000))
 ) ENGINE = ReplacingMergeTree
 PARTITION BY toYYYYMM(received_at) ORDER BY (time,id)
 TTL received_at + INTERVAL 30 DAY DELETE
 SETTINGS fsync_after_insert=1, fsync_part_directory=1`
	_, err := ch.query(ctx, sql, nil, nil)
	return err
}
func (ch *ClickHouse) Ping(ctx context.Context) error {
	_, err := ch.query(ctx, "SELECT 1 FROM ocsf.events LIMIT 0", nil, nil)
	return err
}
func (ch *ClickHouse) Insert(ctx context.Context, events []Event) error {
	var body bytes.Buffer
	for _, event := range events {
		if err := json.NewEncoder(&body).Encode(event); err != nil {
			return err
		}
	}
	_, err := ch.query(ctx, "INSERT INTO ocsf.events (id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw) FORMAT JSONEachRow", nil, body.Bytes())
	return err
}

type Cursor struct {
	Time     int64  `json:"t"`
	ID       string `json:"i"`
	Hash     string `json:"q"`
	Snapshot int64  `json:"s"`
}
type SearchResult struct {
	Events   []Event `json:"events"`
	Total    uint64  `json:"total"`
	Next     string  `json:"next_cursor,omitempty"`
	Snapshot int64   `json:"snapshot_ms"`
}

var fieldPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$`)

func searchSQL(input url.Values) (string, url.Values, int, *Cursor, error) {
	params := url.Values{}
	where := []string{"received_at > now() - INTERVAL 30 DAY"}
	limit := 100
	if s := input.Get("limit"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > 200 {
			return "", nil, 0, nil, fmt.Errorf("limit must be 1–200")
		}
		limit = n
	}
	for _, field := range []string{"class_uid", "severity_id", "start", "end"} {
		if v := input.Get(field); v != "" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 {
				return "", nil, 0, nil, fmt.Errorf("%s must be a nonnegative integer", field)
			}
			params.Set(field, v)
			column, op := field, "="
			if field == "start" {
				column, op = "time", ">="
			}
			if field == "end" {
				column, op = "time", "<="
			}
			where = append(where, column+op+"{"+field+":Int64}")
		}
	}
	if input.Get("start") != "" && input.Get("end") != "" {
		a, _ := strconv.ParseInt(input.Get("start"), 10, 64)
		b, _ := strconv.ParseInt(input.Get("end"), 10, 64)
		if a > b {
			return "", nil, 0, nil, fmt.Errorf("end must follow start")
		}
	}
	for _, field := range []string{"source", "stream"} {
		if v := input.Get(field); v != "" {
			if len(v) > 256 {
				return "", nil, 0, nil, fmt.Errorf("filter too long")
			}
			params.Set(field, v)
			where = append(where, field+"={"+field+":String}")
		}
	}
	if q := input.Get("q"); q != "" {
		if len(q) > 512 {
			return "", nil, 0, nil, fmt.Errorf("search text exceeds 512 bytes")
		}
		params.Set("q", q)
		where = append(where, "positionCaseInsensitiveUTF8(raw,{q:String})>0")
	}
	if field := input.Get("field"); field != "" {
		if len(field) > 128 || !fieldPattern.MatchString(field) {
			return "", nil, 0, nil, fmt.Errorf("invalid field path")
		}
		if len(input.Get("value")) > 512 {
			return "", nil, 0, nil, fmt.Errorf("field value too long")
		}
		params.Set("path", "$."+field)
		params.Set("value", input.Get("value"))
		where = append(where, "JSON_EXISTS(raw,{path:String}) AND JSON_VALUE(raw,{path:String})={value:String}")
	}
	fingerprint := digest([]byte(params.Encode()))
	cursor := &Cursor{Hash: fingerprint, Snapshot: time.Now().UnixMilli()}
	if encoded := input.Get("cursor"); encoded != "" {
		data, e := base64.RawURLEncoding.DecodeString(encoded)
		if e != nil || len(data) > 1024 || json.Unmarshal(data, cursor) != nil || cursor.Hash != fingerprint || len(cursor.ID) != 64 || cursor.Snapshot <= 0 || cursor.Snapshot > time.Now().Add(time.Minute).UnixMilli() {
			return "", nil, 0, nil, fmt.Errorf("invalid cursor; restart the search")
		}
	}
	params.Set("snapshot", strconv.FormatInt(cursor.Snapshot, 10))
	where = append(where, "received_ms<={snapshot:Int64}")
	return strings.Join(where, " AND "), params, limit, cursor, nil
}
func (ch *ClickHouse) Search(ctx context.Context, input url.Values) (SearchResult, error) {
	out := SearchResult{Events: []Event{}}
	where, params, limit, cursor, err := searchSQL(input)
	if err != nil {
		return out, err
	}
	out.Snapshot = cursor.Snapshot
	count, err := ch.query(ctx, "SELECT count() AS n FROM ocsf.events FINAL WHERE "+where+" FORMAT JSONEachRow", params, nil)
	if err != nil {
		return out, err
	}
	var c struct {
		N json.Number `json:"n"`
	}
	if err = json.Unmarshal(bytes.TrimSpace(count), &c); err != nil {
		return out, err
	}
	out.Total, _ = strconv.ParseUint(c.N.String(), 10, 64)
	if cursor.ID != "" {
		where += " AND (time,id)<({cursor_time:Int64},{cursor_id:String})"
		params.Set("cursor_time", strconv.FormatInt(cursor.Time, 10))
		params.Set("cursor_id", cursor.ID)
	}
	// Bound raw bytes as well as row count. Include one sentinel record beyond
	// the byte boundary so callers receive a cursor even for unusually large logs.
	columns := "id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw"
	inner := "SELECT " + columns + " FROM ocsf.events FINAL WHERE " + where + " ORDER BY time DESC,id DESC LIMIT " + strconv.Itoa(limit+1)
	sql := "SELECT " + columns + " FROM (SELECT *, sum(length(raw)) OVER (ORDER BY time DESC,id DESC ROWS UNBOUNDED PRECEDING) AS page_bytes FROM (" + inner + ")) WHERE page_bytes-length(raw)<=4194304 ORDER BY time DESC,id DESC FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers=0"
	data, err := ch.query(ctx, sql, params, nil)
	if err != nil {
		return out, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	pageBytes := 0
	more := false
	for decoder.More() {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			return out, err
		}
		if len(out.Events) >= limit || (len(out.Events) > 0 && pageBytes+len(event.Raw) > 4<<20) {
			more = true
			break
		}
		pageBytes += len(event.Raw)
		out.Events = append(out.Events, event)
	}
	if more {
		last := out.Events[len(out.Events)-1]
		next, _ := json.Marshal(Cursor{Time: last.Time, ID: last.ID, Hash: cursor.Hash, Snapshot: cursor.Snapshot})
		out.Next = base64.RawURLEncoding.EncodeToString(next)
	}
	return out, nil
}
