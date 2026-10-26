package snapshot

import (
	"regexp"
	"strings"
	"unicode"
)

const MaxBytes = 512 * 1024
const SeedMaxBytes = 384 * 1024

var queryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var parameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validated stores immutable canonical UTF-8 bytes as a string, never caller maps.
// The HTTP boundary constructs these via Import; storage persists JSON and Digest together.
type Validated struct {
	JSON   string
	Digest string
}

func Import(source []byte) (Validated, error) {
	if len(source) > MaxBytes {
		return Validated{}, ErrInvalid
	}
	value, err := ParseJSON(source)
	if err != nil {
		return Validated{}, err
	}
	if !validSnapshot(value) {
		return Validated{}, ErrInvalid
	}
	text := canonical(value)
	if len(text) > MaxBytes {
		return Validated{}, ErrInvalid
	}
	return Validated{JSON: text, Digest: digest(text)}, nil
}

func object(value any, keys ...string) (map[string]any, bool) {
	data, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	if len(keys) == 0 {
		return data, true
	}
	if len(data) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if _, exists := data[key]; !exists {
			return nil, false
		}
	}
	return data, true
}

func text(value any, min, max int) bool {
	s, ok := value.(string)
	return ok && len(s) >= min && len(s) <= max
}
func array(value any, min, max int) ([]any, bool) {
	a, ok := value.([]any)
	return a, ok && len(a) >= min && len(a) <= max
}

func validSnapshot(value any) bool {
	data, ok := object(value, "format", "language", "engine", "engineVersion", "title", "schemaSql", "seed", "queries", "activeQueryId")
	if !ok || data["format"] != "querydock-fiddle/1" || data["language"] != "sql" {
		return false
	}
	switch data["engine"] {
	case "postgres", "sqlite", "duckdb":
	default:
		return false
	}
	if !text(data["engineVersion"], 1, 128) || !text(data["title"], 1, 256) || !text(data["schemaSql"], 0, MaxBytes) || !text(data["activeQueryId"], 1, 128) {
		return false
	}
	version := data["engineVersion"].(string)
	// ECMAScript \s includes BOM, and excludes NEL (U+0085).
	if strings.ContainsFunc(version, func(r rune) bool { return r == '\ufeff' || (r != '\u0085' && unicode.IsSpace(r)) }) {
		return false
	}
	switch strings.ToLower(version) {
	case "latest", "unknown", "unresolved", "*":
		return false
	}
	queries, ok := array(data["queries"], 1, 128)
	if !ok {
		return false
	}
	ids := make(map[string]bool)
	for _, value := range queries {
		q, ok := object(value, "id", "name", "sql", "parameters")
		if !ok || !text(q["id"], 1, 128) || !text(q["name"], 1, 256) || !text(q["sql"], 0, MaxBytes) {
			return false
		}
		id := q["id"].(string)
		if !queryID.MatchString(id) || ids[id] {
			return false
		}
		ids[id] = true
		params, ok := object(q["parameters"])
		if !ok || len(params) > 256 {
			return false
		}
		for key, value := range params {
			if len(key) > 128 || !parameterName.MatchString(key) || !validParameter(value) {
				return false
			}
		}
	}
	return ids[data["activeQueryId"].(string)] && validSeed(data["seed"])
}

func validParameter(value any) bool {
	data, ok := object(value, "type", "value")
	if !ok {
		return false
	}
	switch data["type"] {
	case "string":
		return text(data["value"], 0, MaxBytes)
	case "number":
		_, ok := data["value"].(float64)
		return ok
	case "boolean":
		_, ok := data["value"].(bool)
		return ok
	case "null":
		return data["value"] == nil
	default:
		return false
	}
}

func validSeed(value any) bool {
	raw, ok := object(value)
	if !ok {
		return false
	}
	if raw["format"] == "sql" {
		data, ok := object(value, "format", "sql")
		return ok && text(data["sql"], 0, SeedMaxBytes)
	}
	if raw["format"] != "json" && raw["format"] != "csv" {
		return false
	}
	data, ok := object(value, "format", "tables")
	if !ok {
		return false
	}
	tables, ok := array(data["tables"], 0, 128)
	if !ok {
		return false
	}
	names, files := make(map[string]bool), make(map[string]bool)
	total := 0
	for _, value := range tables {
		t, ok := object(value, "table", "fileName", "content", "columns")
		if !ok || !text(t["table"], 1, 256) || !text(t["fileName"], 1, 256) || !text(t["content"], 0, SeedMaxBytes) {
			return false
		}
		name, file := t["table"].(string), t["fileName"].(string)
		if names[name] || files[file] {
			return false
		}
		names[name], files[file] = true, true
		columns, ok := array(t["columns"], 0, 1000)
		if !ok {
			return false
		}
		seen := make(map[string]bool)
		for _, column := range columns {
			if !text(column, 1, 256) {
				return false
			}
			name := column.(string)
			if seen[name] {
				return false
			}
			seen[name] = true
		}
		total += len(t["content"].(string))
	}
	return total <= SeedMaxBytes
}
