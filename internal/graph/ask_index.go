package graph

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/h0rn3t/Graft/internal/fsutil"
	"github.com/h0rn3t/Graft/internal/jsonjs"
	"github.com/h0rn3t/Graft/internal/sourcefiles"
)

// askBag is a term-count bag that remembers first-occurrence order:
// Counts[i] counts Terms[i].
type askBag struct {
	Terms  []string       `json:"terms"`
	Counts []int          `json:"counts"`
	at     map[string]int // index into Terms while the bag is filled
}

func (bag *askBag) add(term string, count int) {
	i, ok := bag.at[term]
	if !ok {
		i = len(bag.Terms)
		bag.at[term] = i
		bag.Terms = append(bag.Terms, term)
		bag.Counts = append(bag.Counts, 0)
	}
	bag.Counts[i] += count
}

// askOrderedCounts tokenizes text like askTermCounts, keeping term order.
func askOrderedCounts(text string) askBag {
	bag := askBag{at: make(map[string]int)}
	for _, term := range askTerms(text) {
		bag.add(term, 1)
	}
	return bag
}

// AskIndexVersion is the ask index format. Version 3 stores terms folded by
// AskFold, with acronyms split from the words they start; an index of any
// other version is treated as absent. The term cache WriteAskIndex keeps
// beside the index is versioned by it too, so bump it whenever a text yields
// different terms.
const AskIndexVersion = 3

// WriteAskIndex stores the lexical body tokens omitted from the persisted graph.
// Texts unchanged since the last write take their bags from the term cache in
// .cache/ask-terms.json instead of being tokenized again.
func WriteAskIndex(outDir string, graph GraphV1) error {
	file, bags := countAskIndex(graph, readAskTermBags(outDir))
	data, err := file.encode()
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(askIndexPath(outDir), data, 0o644); err != nil {
		return fmt.Errorf("write ask index: %w", err)
	}
	// A cache that cannot be written only costs tokenizing every text next time.
	cache := struct {
		Version int               `json:"version"`
		Bags    map[string]askBag `json:"bags"`
	}{Version: AskIndexVersion, Bags: bags}
	if data, err := jsonv2.Marshal(cache, jsonv2.Deterministic(true)); err == nil {
		_ = fsutil.WriteFileAtomic(askTermsPath(outDir), data, 0o644)
	}
	return nil
}

// askIndexFile is ask-index.json before it is encoded: the term bags of each
// ranked node in id order, and how many of them hold each term.
type askIndexFile struct {
	docs    []askIndexDocument
	df      askBag
	average float64
}

type askIndexDocument struct {
	id               string
	name, path, body askBag
}

// countAskIndex counts the terms of graph's ranked nodes, taking the bag of a
// text counted before from bags, and returns the bags of this graph's texts.
func countAskIndex(graph GraphV1, bags map[string]askBag) (askIndexFile, map[string]askBag) {
	next := make(map[string]askBag)
	counts := func(text string) askBag {
		key := sourcefiles.Hash(text)
		bag, ok := bags[key]
		if !ok || len(bag.Terms) != len(bag.Counts) {
			bag = askOrderedCounts(text)
		}
		next[key] = bag
		return bag
	}
	nodes := sortNodesByID(graph.Nodes)
	file := askIndexFile{docs: make([]askIndexDocument, 0, len(nodes)), df: askBag{at: make(map[string]int)}}
	length := 0
	for _, node := range nodes {
		if !askRanked(node) {
			continue
		}
		doc := askIndexDocument{id: node.ID, name: counts(node.Name), path: counts(node.Path), body: counts(askNodeBody(node))}
		file.docs = append(file.docs, doc)
		seen := make(map[string]struct{}, len(doc.name.Terms)+len(doc.path.Terms)+len(doc.body.Terms))
		for _, bag := range []askBag{doc.name, doc.path, doc.body} {
			for _, term := range bag.Terms {
				if _, dup := seen[term]; dup {
					continue
				}
				seen[term] = struct{}{}
				file.df.add(term, 1)
			}
		}
		for _, count := range doc.body.Counts {
			length += count
		}
	}
	if len(file.docs) > 0 {
		file.average = float64(length) / float64(len(file.docs))
	}
	return file, next
}

// encode writes the file as jsonjs.Marshal writes its JSON shape, which spent
// most of a refresh boxing each term pair.
func (file askIndexFile) encode() ([]byte, error) {
	// Like jsonjs.Marshal, AppendQuote writes invalid UTF-8 as U+FFFD; the
	// error only reports that.
	quote := func(data []byte, text string) []byte {
		data, _ = jsontext.AppendQuote(data, text)
		return data
	}
	// Every bag keeps first-occurrence order, as the TypeScript Maps do.
	pairs := func(data []byte, bag askBag) []byte {
		data = append(data, '[')
		for i, term := range bag.Terms {
			if i > 0 {
				data = append(data, ',')
			}
			data = quote(append(data, '['), term)
			data = append(strconv.AppendInt(append(data, ','), int64(bag.Counts[i]), 10), ']')
		}
		return append(data, ']')
	}
	average, err := jsonjs.Marshal(file.average, "")
	if err != nil {
		return nil, fmt.Errorf("encode ask index: %w", err)
	}
	data := fmt.Appendf(nil, `{"version":%d,"avgBodyLen":%s,"df":`, AskIndexVersion, average)
	data = pairs(data, file.df)
	data = append(strconv.AppendInt(append(data, `,"docCount":`...), int64(len(file.docs)), 10), `,"docs":[`...)
	for i, doc := range file.docs {
		if i > 0 {
			data = append(data, ',')
		}
		data = quote(append(data, `{"id":`...), doc.id)
		data = pairs(append(data, `,"name":`...), doc.name)
		data = pairs(append(data, `,"path":`...), doc.path)
		data = append(pairs(append(data, `,"body":`...), doc.body), '}')
	}
	return append(data, "]}\n"...), nil
}

// index returns the file as a query reads it back: nil when two documents
// share an id, since such an index is dropped.
func (file askIndexFile) index() *AskIndex {
	var pairs []askTermCount
	counts := func(bag askBag) AskCounts {
		pairs = pairs[:0]
		for i, term := range bag.Terms {
			pairs = append(pairs, askTermCount{term: term, count: uint32(bag.Counts[i])}) //nolint:gosec // G115: a term occurs no more often than its text has characters
		}
		return packAskCounts(pairs)
	}
	df := make(map[string]int, len(file.df.Terms))
	for i, term := range file.df.Terms {
		df[term] = file.df.Counts[i]
	}
	docs := make(map[string]AskIndexDoc, len(file.docs))
	for _, doc := range file.docs {
		docs[doc.id] = AskIndexDoc{Name: counts(doc.name), Path: counts(doc.path), Body: counts(doc.body)}
	}
	if len(docs) != len(file.docs) {
		return nil
	}
	return &AskIndex{Version: AskIndexVersion, AvgBodyLen: file.average, DF: df, DocCount: len(file.docs), Docs: docs}
}

// ReadAskIndex reads the ask index at path as a query uses it. It returns nil
// when there is no file or the file is not an index of AskIndexVersion; a
// query then counts the terms itself.
func ReadAskIndex(path string) *AskIndex {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	reader := askIndexReader{
		dec:   jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)),
		terms: make(map[string]string),
	}
	index, ok := reader.index()
	if !ok {
		return nil
	}
	return index
}

// askIndexReader decodes ask-index.json token by token, straight into the
// bags a query reads. Decoding it into generic values first took four times
// as long and left several times the garbage.
type askIndexReader struct {
	dec *jsontext.Decoder
	// terms maps a term, as the file quotes it, to the one string shared by
	// every bag that lists the term.
	terms map[string]string
	pairs []askTermCount // the bag read last, in the file's order
}

// next reads a token and reports whether it is of kind.
func (r *askIndexReader) next(kind jsontext.Kind) bool {
	token, err := r.dec.ReadToken()
	return err == nil && token.Kind() == kind
}

// number reads a non-negative integer of at most 32 bits.
func (r *askIndexReader) number() (uint32, bool) {
	value, err := r.dec.ReadValue()
	if err != nil || len(value) == 0 || len(value) > 10 {
		return 0, false
	}
	n := uint64(0)
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		n = n*10 + uint64(digit-'0')
	}
	if n > math.MaxUint32 {
		return 0, false
	}
	return uint32(n), true
}

// bag reads [[term, count], ...] into r.pairs.
func (r *askIndexReader) bag() bool {
	r.pairs = r.pairs[:0]
	if !r.next('[') {
		return false
	}
	for r.dec.PeekKind() == '[' {
		if !r.next('[') {
			return false
		}
		quoted, err := r.dec.ReadValue()
		if err != nil || quoted.Kind() != '"' {
			return false
		}
		term, known := r.terms[string(quoted)]
		if !known {
			// The decoder vouched for the syntax, so all AppendUnquote can
			// report is invalid UTF-8, which it has replaced.
			unquoted, _ := jsontext.AppendUnquote(nil, quoted)
			term = string(unquoted)
			r.terms[string(quoted)] = term
		}
		count, ok := r.number()
		if !ok || term == "" || !r.next(']') {
			return false
		}
		r.pairs = append(r.pairs, askTermCount{term: term, count: count})
	}
	return r.next(']')
}

// docs reads [{"id": ..., "name": ..., "path": ..., "body": ...}, ...]. It
// also returns how many documents the array lists, which is more than the map
// holds when two of them share an id.
func (r *askIndexReader) docs() (map[string]AskIndexDoc, int, bool) {
	if !r.next('[') {
		return nil, 0, false
	}
	docs := make(map[string]AskIndexDoc)
	listed := 0
	for r.dec.PeekKind() == '{' {
		if !r.next('{') {
			return nil, 0, false
		}
		var id string
		var doc AskIndexDoc
		var hasName, hasPath, hasBody bool
		for r.dec.PeekKind() == '"' {
			name, err := r.dec.ReadToken()
			if err != nil {
				return nil, 0, false
			}
			ok := true
			switch name.String() {
			case "id":
				var token jsontext.Token
				if token, err = r.dec.ReadToken(); err != nil || token.Kind() != '"' {
					return nil, 0, false
				}
				id = token.String()
			case "name":
				ok, hasName = r.bag(), true
				doc.Name = packAskCounts(r.pairs)
			case "path":
				ok, hasPath = r.bag(), true
				doc.Path = packAskCounts(r.pairs)
			case "body":
				ok, hasBody = r.bag(), true
				doc.Body = packAskCounts(r.pairs)
			default:
				ok = r.dec.SkipValue() == nil
			}
			if !ok {
				return nil, 0, false
			}
		}
		if !r.next('}') || id == "" || !hasName || !hasPath || !hasBody {
			return nil, 0, false
		}
		docs[id] = doc
		listed++
	}
	return docs, listed, r.next(']')
}

// index reads the file and validates it as a whole.
func (r *askIndexReader) index() (*AskIndex, bool) {
	index := new(AskIndex)
	var hasVersion, hasAverage bool
	docCount, listed := -1, 0
	if !r.next('{') {
		return nil, false
	}
	for r.dec.PeekKind() == '"' {
		name, err := r.dec.ReadToken()
		if err != nil {
			return nil, false
		}
		var ok bool
		switch name.String() {
		case "version":
			var version uint32
			version, ok = r.number()
			index.Version, hasVersion = int(version), true
		case "avgBodyLen":
			var average jsontext.Value
			if average, err = r.dec.ReadValue(); err != nil || average.Kind() != '0' {
				return nil, false
			}
			index.AvgBodyLen, err = strconv.ParseFloat(string(average), 64)
			ok, hasAverage = err == nil, true
		case "df":
			if ok = r.bag(); ok {
				index.DF = make(map[string]int, len(r.pairs))
				for _, pair := range r.pairs {
					index.DF[pair.term] = int(pair.count)
				}
			}
		case "docCount":
			var count uint32
			count, ok = r.number()
			docCount = int(count)
		case "docs":
			index.Docs, listed, ok = r.docs()
		default:
			ok = r.dec.SkipValue() == nil
		}
		if !ok {
			return nil, false
		}
	}
	if !r.next('}') {
		return nil, false
	}
	if _, err := r.dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	if !hasVersion || index.Version != AskIndexVersion || !hasAverage || index.DF == nil || index.Docs == nil ||
		docCount != listed || len(index.Docs) != listed {
		return nil, false
	}
	index.DocCount = docCount
	return index, true
}

// readAskTermBags returns the term cache's bags, keyed by the SHA-256 of the
// text each counts, or an empty map when there is no usable cache.
func readAskTermBags(outDir string) map[string]askBag {
	var cache struct {
		Version int               `json:"version"`
		Bags    map[string]askBag `json:"bags"`
	}
	data, err := os.ReadFile(askTermsPath(outDir))
	if err != nil || jsonv2.Unmarshal(data, &cache) != nil || cache.Version != AskIndexVersion || cache.Bags == nil {
		return make(map[string]askBag)
	}
	return cache.Bags
}

func askTermsPath(outDir string) string {
	return filepath.Join(outDir, ".cache", "ask-terms.json")
}

func askIndexPath(outDir string) string {
	return filepath.Join(outDir, ".cache", "ask-index.json")
}
