package snapshot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vector struct {
	Name, Source, Canonical, SHA256 string
	Snapshot                        map[string]any
}

func vectors(t *testing.T, file string) []vector {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var result []vector
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func base(t *testing.T) map[string]any {
	t.Helper()
	v, err := ParseJSON([]byte(vectors(t, "snapshot-vectors.json")[0].Source))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}
func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestPublishedConformance(t *testing.T) {
	for _, file := range []string{"jcs-vectors.json", "snapshot-vectors.json"} {
		for _, v := range vectors(t, file) {
			t.Run(v.Name, func(t *testing.T) {
				got, err := Canonicalize([]byte(v.Source))
				if err != nil || got != v.Canonical || digest(got) != v.SHA256 {
					t.Fatalf("canonical mismatch: %s %v", got, err)
				}
				if file == "snapshot-vectors.json" {
					validated, err := Import([]byte(v.Source))
					if err != nil || validated.JSON != v.Canonical || validated.Digest != v.SHA256 {
						t.Fatalf("snapshot mismatch %+v %v", validated, err)
					}
					roundtrip, err := Import([]byte(validated.JSON))
					if err != nil || roundtrip != validated {
						t.Fatalf("roundtrip %+v %v", roundtrip, err)
					}
				}
			})
		}
	}
	for _, v := range vectors(t, "rejected-vectors.json") {
		t.Run(v.Name, func(t *testing.T) {
			if _, err := Import([]byte(v.Source)); err == nil {
				t.Fatal("accepted rejected vector")
			}
		})
	}
}
func TestStrictJSON(t *testing.T) {
	invalid := []string{"", "{", "[", "[1,]", "{\"a\"}", "{\"a\":}", "{\"a\":1", "{\"a\":1,}", "{\"a\":1 \"b\":2}", "[1", "[1 2]", "true false", "1e400", "01", `"\u`, `"\uXXXX"`, `"\ud800"`, `"\udc00"`, `"\ud800\u0041"`, `"\ud800\uXXXX"`, `"\ud800x123456"`, "\"a\n\"", `"unterminated`, `{"a":1,"\u0061":2}`, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), string([]byte{'"', 0xff, '"'})}
	for _, source := range invalid {
		t.Run(source, func(t *testing.T) {
			if _, err := ParseJSON([]byte(source)); err == nil {
				t.Fatalf("accepted %q", source)
			}
			if _, err := Canonicalize([]byte(source)); err == nil {
				t.Fatal("canonicalized malformed JSON")
			}
		})
	}
	valid := map[string]string{`"\ud83d\ude00"`: `"😀"`, `"\\ud800"`: `"\\ud800"`, `"\u0041"`: `"A"`, `"/<>\u2028\u2029\b\f\r\u0000\u001f"`: "\"/<>\u2028\u2029\\b\\f\\r\\u0000\\u001f\"", `{"x":[],"y":{},"b":false,"n":null}`: `{"b":false,"n":null,"x":[],"y":{}}`, `[-1e-7,-1e21,1e-6,1e20,1e21,5e-324,1e-400]`: `[-1e-7,-1e+21,0.000001,100000000000000000000,1e+21,5e-324,0]`}
	for source, want := range valid {
		got, err := Canonicalize([]byte(source))
		if err != nil || got != want {
			t.Fatalf("%q -> %q want %q (%v)", source, got, want, err)
		}
	}
}
func mutate(s map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	var target any = s
	for _, part := range parts[:len(parts)-1] {
		if part == "0" {
			target = target.([]any)[0]
		} else {
			target = target.(map[string]any)[part]
		}
	}
	target.(map[string]any)[parts[len(parts)-1]] = value
}
func TestSnapshotMutations(t *testing.T) {
	cases := []struct {
		path  string
		value any
	}{
		{"results", []any{}}, {"format", "querydock-fiddle/2"}, {"language", "graphql"}, {"engine", "mysql"}, {"engineVersion", nil}, {"engineVersion", "18.6\ufeff"}, {"engineVersion", "UnKnown"}, {"title", ""}, {"title", strings.Repeat("é", 129)}, {"schemaSql", true}, {"activeQueryId", false}, {"activeQueryId", "missing"}, {"queries", "query"}, {"queries", []any{}}, {"queries", make([]any, 129)},
		{"queries.0.id", "bad id"}, {"queries.0.name", ""}, {"queries.0.sql", nil}, {"queries.0.parameters", []any{}}, {"queries.0.parameters.bad-name", nil}, {"queries.0.parameters." + strings.Repeat("a", 129), nil}, {"queries.0.parameters.minimum", nil}, {"queries.0.parameters.minimum", map[string]any{"type": "date", "value": nil}}, {"queries.0.parameters.minimum", map[string]any{"type": "number", "value": "0"}}, {"queries.0.parameters.label", map[string]any{"type": "string", "value": false}}, {"queries.0.parameters.enabled", map[string]any{"type": "boolean", "value": 0}}, {"queries.0.parameters.empty", map[string]any{"type": "null", "value": 0}},
		{"seed", nil}, {"seed", map[string]any{"format": "xml"}}, {"seed", map[string]any{"format": "csv", "tables": []any{}, "unknown": 0}}, {"seed", map[string]any{"format": "sql"}}, {"seed.sql", strings.Repeat("x", SeedMaxBytes+1)}, {"seed", map[string]any{"format": "csv", "tables": false}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			s := base(t)
			mutate(s, tc.path, tc.value)
			if _, err := Import(encode(t, s)); err == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	for _, kind := range []string{"missing", "query-shape", "duplicate-query", "param-count"} {
		s := base(t)
		q := s["queries"].([]any)
		switch kind {
		case "missing":
			delete(s, "title")
			s["extra"] = 0
		case "query-shape":
			q[0] = nil
		case "duplicate-query":
			q[1] = q[0]
		case "param-count":
			p := map[string]any{}
			for i := 0; i < 257; i++ {
				p[strings.Repeat("a", i+1)] = nil
			}
			q[0].(map[string]any)["parameters"] = p
		}
		if _, err := Import(encode(t, s)); err == nil {
			t.Fatalf("accepted %s", kind)
		}
	}
	for _, source := range []string{"null", "[]", strings.Repeat(" ", MaxBytes+1)} {
		if _, err := Import([]byte(source)); err == nil {
			t.Fatal("accepted invalid body")
		}
	}
	// The raw request fits, but canonical 1e20 expands to 21 digits.
	s := base(t)
	s["queries"] = []any{map[string]any{"id": "q", "name": "q", "sql": "", "parameters": map[string]any{"n": map[string]any{"type": "number", "value": 0}}}}
	s["activeQueryId"] = "q"
	s["seed"] = map[string]any{"format": "sql", "sql": ""}
	s["schemaSql"] = ""
	s["schemaSql"] = strings.Repeat("x", MaxBytes-len(encode(t, s))-4)
	raw := strings.ReplaceAll(string(encode(t, s)), `"value":0`, `"value":1e20`)
	if len(raw) > MaxBytes {
		t.Fatal("fixture too large before canonicalization")
	}
	if _, err := Import([]byte(raw)); err == nil {
		t.Fatal("accepted oversized canonical snapshot")
	}
}
func TestTableSeeds(t *testing.T) {
	table := func() map[string]any {
		return map[string]any{"table": "boats", "fileName": "boats.csv", "columns": []any{"id", "name"}, "content": "id,name\r\n1,Aurora\r\n"}
	}
	for _, kind := range []string{"shape", "table", "fileName", "content", "columns", "empty-column", "duplicate-column", "duplicate-table", "duplicate-file", "aggregate"} {
		t.Run(kind, func(t *testing.T) {
			a := table()
			tables := []any{a}
			switch kind {
			case "shape":
				tables[0] = nil
			case "table", "fileName":
				a[kind] = ""
			case "content":
				a["content"] = strings.Repeat("x", SeedMaxBytes+1)
			case "columns":
				a["columns"] = false
			case "empty-column":
				a["columns"] = []any{""}
			case "duplicate-column":
				a["columns"] = []any{"id", "id"}
			case "duplicate-table":
				b := table()
				b["fileName"] = "other"
				tables = append(tables, b)
			case "duplicate-file":
				b := table()
				b["table"] = "other"
				tables = append(tables, b)
			case "aggregate":
				b := table()
				a["content"] = strings.Repeat("x", 200000)
				b["content"] = a["content"]
				b["table"] = "other"
				b["fileName"] = "other"
				tables = append(tables, b)
			}
			s := base(t)
			s["seed"] = map[string]any{"format": "csv", "tables": tables}
			if _, err := Import(encode(t, s)); err == nil {
				t.Fatal("accepted invalid table seed")
			}
		})
	}
	for _, engine := range []string{"postgres", "duckdb"} {
		s := base(t)
		s["engine"] = engine
		s["engineVersion"] = "18.6\u0085"
		s["seed"] = map[string]any{"format": "json", "tables": []any{}}
		if _, err := Import(encode(t, s)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFixtureProvenance(t *testing.T) {
	raw, err := os.ReadFile("testdata/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Repository, Commit, Path string
		SHA256                   map[string]string
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Repository != "https://github.com/datatug/datatug-apps" || p.Commit != "2220f62c091488ffa229cbd787550d2c063984fa" || p.Path != "libs/querydock/src/lib" {
		t.Fatal("contract provenance drift")
	}
	for _, file := range []string{"jcs-vectors.json", "rejected-vectors.json", "snapshot-vectors.json"} {
		data, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if digest(string(data)) != p.SHA256["fixtures/"+file] {
			t.Fatalf("copied fixture changed: %s", file)
		}
	}
}
