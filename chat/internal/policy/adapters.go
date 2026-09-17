package policy

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var cryptoRandReader = rand.Reader

// QueryPair is one decoded query parameter in request order.
type QueryPair struct{ Key, Value string }

// parseQSL mirrors urllib.parse.parse_qsl(keep_blank_values=True): fields
// split on '&', empty fields skipped, undecodable escapes kept verbatim.
func parseQSL(query string) []QueryPair {
	var pairs []QueryPair
	for _, field := range strings.Split(query, "&") {
		if field == "" {
			continue
		}
		key, value, _ := strings.Cut(field, "=")
		pairs = append(pairs, QueryPair{unquotePlus(key), unquotePlus(value)})
	}
	return pairs
}

func unquotePlus(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return strings.ReplaceAll(s, "+", " ")
}

// Google Docs.

var googleDocumentID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var googleDocumentPath = regexp.MustCompile(`^/v1/documents/([A-Za-z0-9_-]{1,256})$`)

// GoogleProtectedHost covers alternate Docs/Drive API and web routes.
func GoogleProtectedHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	return host == "docs.google.com" || host == "drive.google.com" || host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com")
}

// GoogleDocsOperation classifies a supported read, returning the operation
// identifier and document ID.
func GoogleDocsOperation(method, path string, query []QueryPair, body []byte) (string, string, error) {
	if method != "GET" || len(body) > 0 {
		return "", "", errors.New("only supported Google Docs reads are available")
	}
	m := googleDocumentPath.FindStringSubmatch(path)
	if m == nil {
		return "", "", errors.New("unsupported Google Docs operation")
	}
	for _, pair := range query {
		if pair.Key == "includeTabsContent" && (pair.Value == "true" || pair.Value == "false") {
			continue
		}
		if pair.Key == "suggestionsViewMode" {
			switch pair.Value {
			case "DEFAULT_FOR_CURRENT_ACCESS", "SUGGESTIONS_INLINE", "PREVIEW_SUGGESTIONS_ACCEPTED", "PREVIEW_WITHOUT_SUGGESTIONS":
				continue
			}
		}
		return "", "", errors.New("unsupported Google Docs query parameter")
	}
	return "google_docs/documents/get", m[1], nil
}

var batchUpdatePath = regexp.MustCompile(`^/v1/documents/([A-Za-z0-9_-]{1,256}):batchUpdate$`)

// Google Sheets: reads of a spreadsheet or its values, and the value and
// structure writes a write grant covers. The spreadsheet ID is the grant's
// document ID, exactly as for Docs.
var sheetsReadPath = regexp.MustCompile(`^/v4/spreadsheets/([A-Za-z0-9_-]{1,256})(?:/values/[^/]{1,512}|/values:batchGet|/values:batchGetByDataFilter)?$`)
var sheetsWritePath = regexp.MustCompile(`^/v4/spreadsheets/([A-Za-z0-9_-]{1,256})(?::batchUpdate|/values/[^/]{1,512}|/values:batchUpdate|/values:batchClear)$`)
var sheetsReadQuery = stringSet("ranges", "includeGridData", "fields", "majorDimension", "valueRenderOption", "dateTimeRenderOption")
var sheetsWriteQuery = stringSet("valueInputOption", "insertDataOption", "includeValuesInResponse", "responseValueRenderOption", "responseDateTimeRenderOption")

// GoogleSheetsOperation classifies a Sheets API request as a read or write
// of one spreadsheet.
func GoogleSheetsOperation(method, path string, query []QueryPair, body []byte) (access, spreadsheet string, err error) {
	if method == "GET" {
		m := sheetsReadPath.FindStringSubmatch(path)
		if m == nil || len(body) > 0 || strings.HasSuffix(path, ":append") || strings.HasSuffix(path, ":clear") {
			return "", "", errors.New("unsupported Google Sheets read")
		}
		for _, pair := range query {
			if !sheetsReadQuery[pair.Key] {
				return "", "", errors.New("unsupported Google Sheets query parameter")
			}
		}
		return "read", m[1], nil
	}
	m := sheetsWritePath.FindStringSubmatch(path)
	appendOrClear := strings.HasSuffix(path, ":append") || strings.HasSuffix(path, ":clear")
	valuesPut := strings.Contains(path, "/values/") && !appendOrClear
	if m == nil || len(body) == 0 || len(body) > 1024*1024 || (method == "PUT" && !valuesPut) || (method == "POST" && valuesPut) || (method != "POST" && method != "PUT") {
		return "", "", errors.New("unsupported Google Sheets write")
	}
	for _, pair := range query {
		if !sheetsWriteQuery[pair.Key] {
			return "", "", errors.New("unsupported Google Sheets query parameter")
		}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil || data == nil {
		return "", "", errors.New("invalid Google Sheets write")
	}
	// Cell values are "write"; spreadsheets:batchUpdate (sheets, ranges,
	// formats, charts, deleting tabs) needs the "structure" level.
	if strings.HasSuffix(path, ":batchUpdate") && !strings.Contains(path, "/values:") {
		return "structure", m[1], nil
	}
	return "write", m[1], nil
}

// AccessRank orders document grant levels: a grant authorizes every
// operation of its own level and below. "create" is a structure-level
// grant on a document Warden created for the agent.
func AccessRank(access string) int {
	switch access {
	case "read":
		return 0
	case "write":
		return 1
	case "structure", "create":
		return 2
	}
	return -1
}

// docsImageEdits fetch a remote URI on Google's side, which would let an
// agent exfiltrate data through the URL; no grant level permits them.
var docsImageEdits = stringSet("insertInlineImage", "replaceImage")

// docsTextEdits are the requests a "write" grant covers; every other
// batchUpdate request (tables, tabs, headers, named ranges, page breaks, ...)
// is "structure".
var docsTextEdits = stringSet("insertText", "deleteContentRange", "updateTextStyle", "updateParagraphStyle")

// DocumentWriteOperation classifies shared-document reads and batchUpdate
// edits as "read", "write" (text and styling) or "structure" (everything
// else). Creation is host-executed after owner approval only.
func DocumentWriteOperation(method, path string, query []QueryPair, body []byte) (access, document string, err error) {
	if strings.HasPrefix(path, "/v4/spreadsheets/") {
		return GoogleSheetsOperation(method, path, query, body)
	}
	if method == "GET" {
		_, doc, err := GoogleDocsOperation(method, path, query, body)
		if err != nil {
			return "", "", err
		}
		return "read", doc, nil
	}
	m := batchUpdatePath.FindStringSubmatch(path)
	if method != "POST" || m == nil || len(query) > 0 || len(body) == 0 || len(body) > 256*1024 {
		return "", "", errors.New("unsupported document write")
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil || data == nil {
		return "", "", errors.New("invalid document edit")
	}
	for key := range data {
		if key != "requests" && key != "writeControl" {
			return "", "", errors.New("invalid document edit")
		}
	}
	edits, ok := data["requests"].([]any)
	if !ok || len(edits) < 1 || len(edits) > 100 {
		return "", "", errors.New("expected 1–100 edits")
	}
	if raw, present := data["writeControl"]; present {
		control, ok := raw.(map[string]any)
		if !ok {
			return "", "", errors.New("unsupported revision control")
		}
		for key := range control {
			if key != "requiredRevisionId" {
				return "", "", errors.New("unsupported revision control")
			}
		}
		if len(control) > 0 {
			revision, ok := control["requiredRevisionId"].(string)
			if !ok || len(revision) < 1 || len(revision) > 256 {
				return "", "", errors.New("invalid revision")
			}
		}
	}
	access = "write"
	for _, item := range edits {
		edit, ok := item.(map[string]any)
		if !ok || len(edit) != 1 {
			return "", "", errors.New("invalid edit")
		}
		for key, value := range edit {
			if _, isObject := value.(map[string]any); !isObject || docsImageEdits[key] {
				return "", "", errors.New("remote image edits are not allowed")
			}
			if !docsTextEdits[key] {
				access = "structure"
			}
		}
	}
	return access, m[1], nil
}

// Figma.

var figmaFileKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var figmaFilePath = regexp.MustCompile(`^/v1/files/([A-Za-z0-9_-]{1,128})(?:/(nodes|meta|comments))?$`)
var figmaDepth = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
var figmaIDs = regexp.MustCompile(`^[0-9]+:[0-9]+(?:,[0-9]+:[0-9]+)*$`)
var figmaVersion = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// FigmaProtectedHost covers Figma API, web and user content hosts.
func FigmaProtectedHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	for _, suffix := range []string{"figma.com", "figma-gov.com", "figmausercontent.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// FigmaOperation classifies a supported read, returning the operation
// identifier and file key.
func FigmaOperation(method, path string, query []QueryPair, body []byte) (string, string, error) {
	if method != "GET" || len(body) > 0 {
		return "", "", errors.New("only supported Figma reads are available")
	}
	var name, key string
	allowed := map[string]bool{}
	if path == "/v1/me" {
		name = "figma/users/me"
	} else {
		m := figmaFilePath.FindStringSubmatch(path)
		if m == nil {
			return "", "", errors.New("unsupported Figma operation")
		}
		key = m[1]
		suffix := m[2]
		if suffix == "" {
			name = "figma/files/read"
		} else {
			name = "figma/files/" + suffix
		}
		switch suffix {
		case "", "nodes":
			allowed = map[string]bool{"version": true, "ids": true, "depth": true}
		case "comments":
			allowed = map[string]bool{"as_md": true}
		}
		if suffix == "nodes" {
			ids := ""
			for _, pair := range query {
				if pair.Key == "ids" {
					ids = pair.Value
				}
			}
			if ids == "" {
				return "", "", errors.New("Figma node reads require ids")
			}
		}
	}
	for _, pair := range query {
		if !allowed[pair.Key] {
			return "", "", errors.New("unsupported Figma query parameter")
		}
		switch pair.Key {
		case "depth":
			if !figmaDepth.MatchString(pair.Value) {
				return "", "", errors.New("invalid Figma depth")
			}
		case "ids":
			if !figmaIDs.MatchString(pair.Value) {
				return "", "", errors.New("invalid Figma node ids")
			}
		case "version":
			if !figmaVersion.MatchString(pair.Value) {
				return "", "", errors.New("invalid Figma version")
			}
		case "as_md":
			if pair.Value != "true" && pair.Value != "false" {
				return "", "", errors.New("invalid Figma comment format")
			}
		}
	}
	return name, key, nil
}
