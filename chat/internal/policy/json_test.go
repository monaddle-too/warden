package policy

import (
	"encoding/json"
	"testing"
)

func TestDumpsMatchesPythonCanonicalForm(t *testing.T) {
	value := map[string]any{"a": []any{json.Number("1"), 2.5, "é", " ", true, nil, 1000.0, 1e16, 1.5e-7, "\x01\x7f\"\\/"}, "b": map[string]any{"z": json.Number("1"), "y": "x"}}
	bs := string(rune(92))
	want := `{"a":[1,2.5,"` + bs + `u00e9","` + bs + `u2028",true,null,1000.0,1e+16,1.5e-07,"` + bs + `u0001` + bs + `u007f\"\\/"],"b":{"y":"x","z":1}}`
	if got := Dumps(value); got != want {
		t.Fatalf("Dumps mismatch:\n got %s\nwant %s", got, want)
	}
	if got := Dumps("\U0001F600"); got != `"`+bs+`ud83d`+bs+`ude00"` {
		t.Fatalf("surrogate pair: %s", got)
	}
	identity := map[string]string{"generation": "1", "principalID": "owner", "projectID": "p1", "runtimeName": "sbx-one", "sandboxID": "s1"}
	if got := BindingDigest(identity); got != bindingDigestFromPython {
		t.Fatalf("binding digest %s does not match the Python broker's directory name", got)
	}
}

// bindingDigestFromPython is sha256 of the Python json.dumps form of the
// identity above; it names existing sandbox directories on disk.
const bindingDigestFromPython = "45b4bdadd08bc2136a248ff501b29ce6e67a91d95aa876c9c22d5ddb2edbc77a"

func TestStrictJSONRejectsDuplicatesAndTrailingData(t *testing.T) {
	if _, err := StrictJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if _, err := StrictJSON([]byte(`{"x":NaN}`)); err == nil {
		t.Fatal("NaN accepted")
	}
	if _, err := StrictJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	value, err := StrictJSON([]byte(`{"n":1000,"f":1.0,"s":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if Dumps(value) != `{"f":1.0,"n":1000,"s":"x"}` {
		t.Fatalf("round trip changed numbers: %s", Dumps(value))
	}
	if _, ok := asInt(value.(map[string]any)["f"]); ok {
		t.Fatal("float accepted as int")
	}
}

func TestWithoutBodiesAndRedaction(t *testing.T) {
	r := NewRedactor()
	r.Register("ghp_SECRETVALUE1234567890")
	cleaned := r.Clean(map[string]any{"password": "x", "nested": map[string]any{"api_key": "y", "note": "token ghp_SECRETVALUE1234567890 here"}, "token_configured": true}).(map[string]any)
	if cleaned["password"] != "[REDACTED]" || cleaned["nested"].(map[string]any)["api_key"] != "[REDACTED]" || cleaned["token_configured"] != true {
		t.Fatalf("clean: %v", cleaned)
	}
	if cleaned["nested"].(map[string]any)["note"] != "token [REDACTED] here" {
		t.Fatalf("secret survived: %v", cleaned)
	}
	if r.Text("Bearer abc.def") != "[REDACTED]" {
		t.Fatal("bearer pattern not redacted")
	}
	headers := r.Headers([][]string{{"Authorization", "Bearer secret"}, {"x-arbitrary", "secret"}, {"Content-Type", "text/plain"}})
	if headers[0][1] != "[REDACTED]" || headers[1][1] != "[REDACTED]" || headers[2][1] != "text/plain" {
		t.Fatalf("headers: %v", headers)
	}
	stripped := WithoutBodies(map[string]any{"request": map[string]any{"body": map[string]any{"bytes": json.Number("5"), "text": "hidden"}, "body_base64": "aGk="}}).(map[string]any)
	body := stripped["request"].(map[string]any)["body"].(map[string]any)
	if body["capture"] != "omitted_policy" || body["text"] != nil || stripped["request"].(map[string]any)["body_base64"] != "[OMITTED]" {
		t.Fatalf("without bodies: %v", stripped)
	}
	if !GitHubHost("api.github.com") || !GitHubHost("raw.githubusercontent.com") || GitHubHost("github.com.evil.example") || GitHubHost("notgithub.com") {
		t.Fatal("github suffix boundary")
	}
}
