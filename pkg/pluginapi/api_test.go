package pluginapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestCredentialsRedacted(t *testing.T) {
	c := Credentials{"bot_token": "synthetic-secret"}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%#v", c), string(mustJSON(c))} {
		if strings.Contains(s, "synthetic-secret") {
			t.Fatal("credential disclosed")
		}
	}
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func TestManifestValidation(t *testing.T) {
	for _, id := range []string{"", "Bad", "a", "../escape"} {
		if ValidateManifest(Manifest{ID: id, Name: "Fixture", Version: "1", APIVersion: 1}) == nil {
			t.Fatal(id)
		}
	}
	if err := ValidateManifest(Manifest{ID: "fixture", Name: "Fixture", Version: "1", APIVersion: 1}); err != nil {
		t.Fatal(err)
	}
}
func TestSchemaSubset(t *testing.T) {
	for _, s := range []string{`{"type":"object","properties":{"key":{"type":"string","x-secret":true}}}`, `{"type":"array","items":{"type":"string"}}`, `{"type":"string","examples":["fixture","custom"]}`} {
		if err := ValidateSchema(json.RawMessage(s)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{`{"$ref":"outside"}`, `{"type":"array","items":{"type":"object"}}`, `{"type":"object","additionalProperties":true}`, `[]`, `{"type":"string","examples":"invalid"}`, `{"type":"string","examples":[42]}`} {
		if ValidateSchema(json.RawMessage(s)) == nil {
			t.Fatal(s)
		}
	}
}
func TestRedactingLogger(t *testing.T) {
	var out strings.Builder
	logger := RedactingLogger(slog.New(slog.NewTextHandler(&out, nil)), Credentials{"bot_token": "synthetic-secret"})
	logger.Info("payload synthetic-secret", "token", "synthetic-secret", "group", slog.GroupValue(slog.String("nested", "prefix synthetic-secret suffix")))
	if strings.Contains(out.String(), "synthetic-secret") || !strings.Contains(out.String(), "redacted") {
		t.Fatal(out.String())
	}
}
func TestAttachmentReferenceSerializable(t *testing.T) {
	b := Blob{Size: 0, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	encoded, e := json.Marshal(b)
	if e != nil {
		t.Fatal("contract attachment is not serializable", e)
	}
	var restored Blob
	if json.Unmarshal(encoded, &restored) != nil {
		t.Fatal("decode")
	}
	r, e := restored.Open()
	if e != nil {
		t.Fatal(e)
	}
	r.Close()
}
