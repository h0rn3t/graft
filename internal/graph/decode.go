package graph

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
)

// decodeGraph decodes the text of a wiring.json. A file in the shape Write
// gives it is read token by token, each repeated string stored once; any
// other file goes through encoding/json, so the graph is always the one
// json.Unmarshal returns.
func decodeGraph(data []byte) (*GraphV1, error) {
	if graph, ok := readGraph(data); ok {
		return graph, nil
	}
	var graph *GraphV1
	if err := json.Unmarshal(data, &graph); err != nil {
		return nil, err
	}
	return graph, nil
}

// readGraph reads data token by token. It reports false for a file a
// graphReader does not read as encoding/json would.
func readGraph(data []byte) (*GraphV1, bool) {
	reader := graphReader{
		// The reader gives up on a repeated name itself, which costs less
		// than having the decoder track the names of every object.
		dec:    jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true)),
		shared: make(map[string]string),
		most:   len(data) / len("{}"),
	}
	graph := new(GraphV1)
	if !reader.object(graph, (*graphReader).graphMember) {
		return nil, false
	}
	_, err := reader.dec.ReadToken()
	return graph, errors.Is(err, io.EOF)
}

// graphReader reads a wiring.json straight into a GraphV1. Each method
// reports false for anything it does not read exactly as encoding/json
// would: a name it does not know, a name that comes twice, a value of
// another type.
type graphReader struct {
	dec *jsontext.Decoder
	// shared holds the one copy of each string many nodes and edges repeat:
	// an edge's endpoints are node IDs, and a file's nodes share its path.
	shared map[string]string
	// most is the largest number of items a list of the file can hold. It
	// bounds the room a list is given on the word of the file's own counts.
	most int
}

func (r *graphReader) graphMember(graph *GraphV1, name jsontext.Value) (bit int, ok bool) {
	switch string(name) {
	case `"meta"`:
		meta, err := r.dec.ReadValue()
		return 1 << 0, err == nil && json.Unmarshal(meta, &graph.Meta) == nil
	case `"nodes"`:
		return 1 << 1, r.list(&graph.Nodes, graph.Meta.NodeCount, (*graphReader).nodeMember)
	case `"edges"`:
		return 1 << 2, r.list(&graph.Edges, graph.Meta.EdgeCount, (*graphReader).edgeMember)
	}
	return 0, false
}

func (r *graphReader) nodeMember(node *NodeV1, name jsontext.Value) (bit int, ok bool) {
	switch string(name) {
	case `"id"`:
		return 1 << 0, r.sharedText(&node.ID)
	case `"name"`:
		return 1 << 1, r.text(&node.Name)
	case `"kind"`:
		return 1 << 2, r.sharedText((*string)(&node.Kind))
	case `"path"`:
		return 1 << 3, r.sharedText(&node.Path)
	case `"span"`:
		return 1 << 4, r.text(&node.Span)
	case `"signature"`:
		return 1 << 5, r.optional(&node.Signature, (*graphReader).text)
	case `"exported"`:
		return 1 << 6, r.boolean(&node.Exported)
	case `"origin"`:
		return 1 << 7, r.sharedText((*string)(&node.Origin))
	case `"body_hash"`:
		return 1 << 8, r.text(&node.BodyHash)
	case `"chars"`:
		return 1 << 9, r.optional(&node.Chars, (*graphReader).integer)
	case `"body_text"`:
		return 1 << 10, r.optional(&node.BodyText, (*graphReader).text)
	case `"summary_state"`:
		return 1 << 11, r.sharedText((*string)(&node.SummaryState))
	case `"summary"`:
		return 1 << 12, r.optional(&node.Summary, (*graphReader).text)
	case `"crux"`:
		return 1 << 13, r.optional(&node.Crux, (*graphReader).crux)
	case `"owner"`:
		return 1 << 14, r.optional(&node.Owner, (*graphReader).sharedText)
	case `"arity"`:
		return 1 << 15, r.optional(&node.Arity, (*graphReader).integer)
	case `"variadic"`:
		return 1 << 16, r.optional(&node.Variadic, (*graphReader).boolean)
	case `"complexity"`:
		return 1 << 17, r.optional(&node.Complexity, (*graphReader).integer)
	case `"generated"`:
		return 1 << 18, r.boolean(&node.Generated)
	}
	return 0, false
}

func (r *graphReader) crux(crux *Crux) bool {
	return r.object(crux, (*graphReader).cruxMember)
}

func (r *graphReader) cruxMember(crux *Crux, name jsontext.Value) (bit int, ok bool) {
	switch string(name) {
	case `"code"`:
		return 1 << 0, r.text(&crux.Code)
	case `"span"`:
		return 1 << 1, r.text(&crux.Span)
	}
	return 0, false
}

func (r *graphReader) edgeMember(edge *EdgeV1, name jsontext.Value) (bit int, ok bool) {
	switch string(name) {
	case `"source"`:
		return 1 << 0, r.sharedText(&edge.Source)
	case `"target"`:
		return 1 << 1, r.sharedText(&edge.Target)
	case `"relation"`:
		return 1 << 2, r.sharedText((*string)(&edge.Relation))
	case `"confidence"`:
		return 1 << 3, r.sharedText((*string)(&edge.Confidence))
	case `"line"`:
		return 1 << 4, r.integer(&edge.Line)
	}
	return 0, false
}

// object reads {"name": value, ...} into dst. member reads the value of one
// name, as the file quotes it, and returns the bit that stands for the name.
func (r *graphReader) object[T any](dst *T, member func(*graphReader, *T, jsontext.Value) (int, bool)) bool {
	if !r.next('{') {
		return false
	}
	seen := 0
	for r.dec.PeekKind() == '"' {
		name, err := r.dec.ReadValue()
		if err != nil {
			return false
		}
		bit, ok := member(r, dst, name)
		if !ok || seen&bit != 0 {
			return false
		}
		seen |= bit
	}
	return r.next('}')
}

// list reads [{...}, ...] into *dst, which the file's meta says holds count
// items; null leaves it nil.
func (r *graphReader) list[T any](dst *[]T, count int, member func(*graphReader, *T, jsontext.Value) (int, bool)) bool {
	if r.dec.PeekKind() == 'n' {
		return r.next('n')
	}
	if !r.next('[') {
		return false
	}
	items := make([]T, 0, min(max(count, 0), r.most))
	for r.dec.PeekKind() == '{' {
		items = append(items, *new(T))
		if !r.object(&items[len(items)-1], member) {
			return false
		}
	}
	*dst = items
	return r.next(']')
}

// optional reads a value into a new T; null leaves *dst nil.
func (r *graphReader) optional[T any](dst **T, read func(*graphReader, *T) bool) bool {
	if r.dec.PeekKind() == 'n' {
		return r.next('n')
	}
	*dst = new(T)
	return read(r, *dst)
}

// next reads a token and reports whether it is of kind.
func (r *graphReader) next(kind jsontext.Kind) bool {
	token, err := r.dec.ReadToken()
	return err == nil && token.Kind() == kind
}

// quoted reads a string as the file quotes it, or nil for null.
func (r *graphReader) quoted() (quoted jsontext.Value, ok bool) {
	value, err := r.dec.ReadValue()
	if err != nil {
		return nil, false
	}
	switch value.Kind() {
	case 'n':
		return nil, true
	case '"':
		return value, true
	}
	return nil, false
}

// text reads a string into *dst; null leaves it as it is.
func (r *graphReader) text(dst *string) bool {
	quoted, ok := r.quoted()
	if ok && quoted != nil {
		*dst, ok = unquoted(quoted)
	}
	return ok
}

// sharedText reads a string into *dst as text does, giving every reading of
// the same string one copy.
func (r *graphReader) sharedText(dst *string) bool {
	quoted, ok := r.quoted()
	if !ok || quoted == nil {
		return ok
	}
	if bytes.IndexByte(quoted, '\\') < 0 {
		if text, known := r.shared[string(quoted[1:len(quoted)-1])]; known {
			*dst = text
			return true
		}
	}
	text, ok := unquoted(quoted)
	if ok {
		r.shared[text] = text
		*dst = text
	}
	return ok
}

// unquoted returns the string a quoted one stands for.
func unquoted(quoted jsontext.Value) (string, bool) {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return string(quoted[1 : len(quoted)-1]), true
	}
	text, err := jsontext.AppendUnquote(nil, quoted)
	return string(text), err == nil
}

// integer reads a whole number of at most nine digits into *dst; null
// leaves it as it is. A fraction, an exponent or a longer number is one
// encoding/json may refuse, so it is not read here.
func (r *graphReader) integer(dst *int) bool {
	value, err := r.dec.ReadValue()
	if err != nil {
		return false
	}
	if value.Kind() == 'n' {
		return true
	}
	digits := bytes.TrimPrefix(value, []byte("-"))
	if value.Kind() != '0' || len(digits) > 9 {
		return false
	}
	n := 0
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return false
		}
		n = n*10 + int(digit-'0')
	}
	if len(digits) < len(value) {
		n = -n
	}
	*dst = n
	return true
}

// boolean reads true or false into *dst; null leaves it as it is.
func (r *graphReader) boolean(dst *bool) bool {
	token, err := r.dec.ReadToken()
	if err != nil {
		return false
	}
	switch token.Kind() {
	case 't', 'f':
		*dst = token.Kind() == 't'
	case 'n':
	default:
		return false
	}
	return true
}
