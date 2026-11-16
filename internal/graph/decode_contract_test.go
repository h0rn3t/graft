package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// sampleGraph returns a graph shaped like a built one: files holding
// functions, methods and fields, with calls among them.
func sampleGraph(files, edges int) GraphV1 {
	var graph GraphV1
	for f := range files {
		path := fmt.Sprintf("internal/pkg%d/file%d.go", f%7, f)
		graph.Nodes = append(graph.Nodes, NodeV1{
			ID: path, Name: filepath.Base(path), Kind: "file", Path: path, Span: "L1-L400",
			Origin: "ast", BodyHash: fmt.Sprintf("%064x", f), Chars: new(9000 + f), SummaryState: "pending",
		})
		for s := range 16 {
			name := fmt.Sprintf("Symbol%d", s)
			node := NodeV1{
				ID: path + "#" + name, Name: name, Kind: "function", Path: path,
				Span:      fmt.Sprintf("L%d-L%d", 10*s+1, 10*s+9),
				Signature: new("func " + name + "(ctx context.Context, id string) (*Item, error)"),
				Exported:  s%2 == 0, Origin: "ast", BodyHash: fmt.Sprintf("%064x", f*16+s),
				SummaryState: "pending", Arity: new(2), Complexity: new(1 + s%9),
			}
			switch s % 4 {
			case 1:
				node.Kind, node.Owner = "method", new("Store")
				node.Summary = new(name + " loads the item \"id\" names,\nor reports that it is gone.")
			case 2:
				node.Kind, node.Owner, node.Signature, node.Arity, node.Complexity = "field", new("Store"), nil, nil, nil
			case 3:
				node.Crux = &Crux{Code: "return s.items[id], nil", Span: fmt.Sprintf("L%d", 10*s+5)}
				node.Variadic = new(true)
				node.Generated = f%5 == 0
			}
			graph.Nodes = append(graph.Nodes, node)
		}
	}
	for e := range edges {
		source, target := graph.Nodes[e%len(graph.Nodes)], graph.Nodes[(e*31+7)%len(graph.Nodes)]
		edge := EdgeV1{Source: source.ID, Target: target.ID, Relation: "calls", Confidence: "extracted", Line: 1 + e%300}
		if e%5 == 0 {
			edge.Relation, edge.Line = "contains", 0
		}
		graph.Edges = append(graph.Edges, edge)
	}
	graph.Meta = GraphMeta{
		Version: 1, NodeCount: len(graph.Nodes), EdgeCount: len(graph.Edges), Languages: []string{"go"},
		Scopes:          &[]ScopeV1{{Prefix: "", Markers: []string{"go.mod"}}},
		UnresolvedNames: map[string]UnresolvedName{"Close": {Untyped: 3}},
	}
	return graph
}

// writtenGraph returns the bytes Write stores for graph.
func writtenGraph(tb testing.TB, graph GraphV1) []byte {
	tb.Helper()
	path, err := Write(graph, tb.TempDir())
	if err != nil {
		tb.Fatalf("Write() error = %v, want nil", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("os.ReadFile(%q) error = %v, want nil", path, err)
	}
	return data
}

const everyFieldGraph = `{"meta":{"version":1,"nodeCount":1,"edgeCount":1,"languages":["go"]},
"nodes":[{"id":"a.go#F","name":"F","kind":"function","path":"a.go","span":"L1-L2","signature":"func F()",
"exported":true,"origin":"ast","body_hash":"h","chars":7,"body_text":"x","summary_state":"pending","summary":"s",
"crux":{"code":"c","span":"L1"},"owner":"T","arity":2,"variadic":true,"complexity":3,"generated":true}],
"edges":[{"source":"a.go#F","target":"a.go#G","relation":"calls","confidence":"extracted","line":4}]}`

// readCases are wiring.json texts, one per way a file can differ from what
// Write produces. Read must treat each exactly as encoding/json does.
var readCases = map[string]string{
	"every field":       everyFieldGraph,
	"empty object":      `{}`,
	"empty lists":       `{"nodes":[],"edges":[]}`,
	"false counts":      `{"meta":{"nodeCount":4611686018427387904,"edgeCount":-5},"nodes":[{"id":"a"}],"edges":[{"source":"a"}]}`,
	"null lists":        `{"meta":null,"nodes":null,"edges":null}`,
	"null values":       `{"nodes":[{"id":null,"exported":null,"signature":null,"chars":null,"crux":null,"variadic":null}],"edges":[{"source":null,"line":null}]}`,
	"escapes":           `{"nodes":[{"id":"a\"b","name":"é\n\t\\","summary":"😀"}],"edges":[{"source":"a\"b","target":"a"b"}]}`,
	"negative line":     `{"edges":[{"line":-3}]}`,
	"negative zero":     `{"edges":[{"line":-0}]}`,
	"edges before meta": `{"edges":[{"source":"a"}],"nodes":[{"id":"a"}],"meta":{"nodeCount":1}}`,
	"unknown key":       `{"nodes":[{"id":"a","extra":{"deep":[1,2]}}],"later":true}`,
	"case-folded key":   `{"Nodes":[{"ID":"a","Body_Hash":"h"}],"EDGES":[{"SOURCE":"a"}]}`,
	"duplicate key":     `{"nodes":[{"id":"a","id":"b"}]}`,
	"duplicate crux":    `{"nodes":[{"crux":{"code":"a"},"crux":{"span":"b"}}]}`,
	"duplicate null":    `{"nodes":[{"signature":"a","signature":null,"arity":2,"arity":null}]}`,
	"duplicate list":    `{"nodes":[{"id":"a"}],"nodes":[{"id":"b"}]}`,
	"duplicate meta":    `{"meta":{"version":1},"meta":{"nodeCount":2}}`,
	"null element":      `{"nodes":[null,{"id":"a"}],"edges":[null]}`,
	"fraction":          `{"edges":[{"line":1.5}]}`,
	"exponent":          `{"nodes":[{"arity":1e2}]}`,
	"huge number":       `{"edges":[{"line":123456789012345678901234567890}]}`,
	"string for bool":   `{"nodes":[{"exported":"yes"}]}`,
	"number for string": `{"nodes":[{"id":7}]}`,
	"object for list":   `{"nodes":{"id":"a"}}`,
	"meta wrong type":   `{"meta":{"version":"x"},"nodes":[{"id":"a"}]}`,
	"invalid UTF-8":     "{\"nodes\":[{\"name\":\"\xff\"}]}",
	"lone surrogate":    `{"nodes":[{"name":"\ud800"}]}`,
	"trailing data":     `{"nodes":[]} x`,
	"truncated":         `{"meta":{},"nodes":[{"id":"a"}`,
	"null":              `null`,
	"list":              `[]`,
	"empty":             ``,
}

// checkReadMatchesJSON fails unless Read gives data's graph as encoding/json
// decodes it, or an error where encoding/json gives one or no graph at all.
func checkReadMatchesJSON(t *testing.T, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), GraphFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}
	var want *GraphV1
	wantErr := json.Unmarshal(data, &want)
	got, err := Read(path)
	if wantErr != nil || want == nil {
		if err == nil {
			t.Errorf("Read(%q) error = nil, want an error (encoding/json: %v)", data, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("Read(%q) error = %v, want nil", data, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read(%q) = %+v, want %+v", data, got, want)
	}
}

func TestReadMatchesJSON(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{"written": writtenGraph(t, sampleGraph(3, 40))}
	for name, text := range readCases {
		cases[name] = []byte(text)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkReadMatchesJSON(t, data)
		})
	}
}

// A graph Write stores, and one with every field of the model set, must be
// read by the token reader: a field added to the model without a case there
// would send every graph through encoding/json and nothing else would show it.
func TestGraphReaderReadsEveryField(t *testing.T) {
	t.Parallel()
	every := GraphV1{Nodes: make([]NodeV1, 1), Edges: make([]EdgeV1, 1)}
	fillFields(reflect.ValueOf(&every.Nodes[0]).Elem())
	fillFields(reflect.ValueOf(&every.Edges[0]).Elem())
	everyField, err := json.Marshal(every)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	for name, data := range map[string][]byte{
		"written":     writtenGraph(t, sampleGraph(3, 40)),
		"every field": everyField,
	} {
		got, ok := readGraph(data)
		if !ok {
			t.Errorf("readGraph(%s) gave up on %s, want it read", name, data)
			continue
		}
		var want *GraphV1
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatalf("json.Unmarshal(%s) error = %v, want nil", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("readGraph(%s) = %+v, want %+v", name, got, want)
		}
	}
}

// fillFields sets every field of a struct, through pointers and nested
// structs, to a value its JSON form shows.
func fillFields(value reflect.Value) {
	switch value.Kind() {
	case reflect.Struct:
		for _, field := range value.Fields() {
			fillFields(field)
		}
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillFields(value.Elem())
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int:
		value.SetInt(7)
	}
}

func FuzzRead(f *testing.F) {
	f.Add(writtenGraph(f, sampleGraph(1, 6)))
	for _, text := range readCases {
		f.Add([]byte(text))
	}
	f.Fuzz(checkReadMatchesJSON)
}
