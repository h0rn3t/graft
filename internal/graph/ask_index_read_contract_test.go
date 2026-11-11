package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// ReadAskIndex accepts exactly the file WriteAskIndex writes: anything else
// is no index, and a query counts the terms itself.

func TestReadAskIndexContract(t *testing.T) {
	const doc = `{"id":"a.go#Run","name":[["run",1]],"path":[["a",1],["go",1]],"body":[["run",2],["fast",0],["run",3]]}`
	tests := []struct {
		name string
		file string
		want bool
	}{
		{name: "an index", file: `{"version":3,"avgBodyLen":2.5,"df":[["run",1],["a",1]],"docCount":1,"docs":[` + doc + `]}` + "\n", want: true},
		{name: "keys in another order and unknown keys", file: `{"docs":[{"body":[],"extra":{"x":[1]},"path":[],"name":[],"id":"a.go#Run"}],"note":null,"docCount":1,"df":[],"avgBodyLen":0,"version":3}`, want: true},
		{name: "no documents", file: `{"version":3,"avgBodyLen":0,"df":[],"docCount":0,"docs":[]}`, want: true},
		{name: "another version", file: `{"version":2,"avgBodyLen":2.5,"df":[],"docCount":1,"docs":[` + doc + `]}`},
		{name: "fractional version", file: `{"version":3.0,"avgBodyLen":2.5,"df":[],"docCount":1,"docs":[` + doc + `]}`},
		{name: "no version", file: `{"avgBodyLen":2.5,"df":[],"docCount":1,"docs":[` + doc + `]}`},
		{name: "no average", file: `{"version":3,"df":[],"docCount":1,"docs":[` + doc + `]}`},
		{name: "null average", file: `{"version":3,"avgBodyLen":null,"df":[],"docCount":1,"docs":[` + doc + `]}`},
		{name: "no df", file: `{"version":3,"avgBodyLen":2.5,"docCount":1,"docs":[` + doc + `]}`},
		{name: "null df", file: `{"version":3,"avgBodyLen":2.5,"df":null,"docCount":1,"docs":[` + doc + `]}`},
		{name: "null docs", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docCount":0,"docs":null}`},
		{name: "fewer documents than counted", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docCount":2,"docs":[` + doc + `]}`},
		{name: "no document count", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docs":[` + doc + `]}`},
		{name: "two documents with one id", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docCount":2,"docs":[` + doc + `,` + doc + `]}`},
		{name: "document without an id", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docCount":1,"docs":[{"id":"","name":[],"path":[],"body":[]}]}`},
		{name: "document without a body", file: `{"version":3,"avgBodyLen":2.5,"df":[],"docCount":1,"docs":[{"id":"x","name":[],"path":[]}]}`},
		{name: "pair of three", file: `{"version":3,"avgBodyLen":2.5,"df":[["run",1,1]],"docCount":0,"docs":[]}`},
		{name: "pair of one", file: `{"version":3,"avgBodyLen":2.5,"df":[["run"]],"docCount":0,"docs":[]}`},
		{name: "empty term", file: `{"version":3,"avgBodyLen":2.5,"df":[["",1]],"docCount":0,"docs":[]}`},
		{name: "negative count", file: `{"version":3,"avgBodyLen":2.5,"df":[["run",-1]],"docCount":0,"docs":[]}`},
		{name: "fractional count", file: `{"version":3,"avgBodyLen":2.5,"df":[["run",1.5]],"docCount":0,"docs":[]}`},
		{name: "count in quotes", file: `{"version":3,"avgBodyLen":2.5,"df":[["run","1"]],"docCount":0,"docs":[]}`},
		{name: "text after the index", file: `{"version":3,"avgBodyLen":0,"df":[],"docCount":0,"docs":[]} {}`},
		{name: "cut short", file: `{"version":3,"avgBodyLen":2.5,"df":[["run",1]],"docCount":1,"docs":[{"id":"x","name":[["ru`},
		{name: "not an object", file: `[]`},
		{name: "empty file", file: ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ask-index.json")
			if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ReadAskIndex(path); (got != nil) != tt.want {
				t.Errorf("ReadAskIndex(%s) = %+v, want an index: %v", tt.file, got, tt.want)
			}
		})
	}
	if got := ReadAskIndex(filepath.Join(t.TempDir(), "absent.json")); got != nil {
		t.Errorf("ReadAskIndex(absent file) = %+v, want nil", got)
	}
}

func TestReadAskIndexReadsWhatTheFileCounts(t *testing.T) {
	const file = `{"version":3,"avgBodyLen":2.5,"df":[["run",7],["a",1],["run",9]],"docCount":1,"docs":[` +
		`{"id":"a.go#Run","name":[["run",1]],"path":[["a",1],["go",1]],"body":[["run",2],["fast",0],["run",3]]}]}`
	path := filepath.Join(t.TempDir(), "ask-index.json")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	index := ReadAskIndex(path)
	if index == nil {
		t.Fatalf("ReadAskIndex(%s) = nil, want an index", file)
	}
	if index.Version != 3 || index.AvgBodyLen != 2.5 || index.DocCount != 1 || len(index.Docs) != 1 {
		t.Errorf("ReadAskIndex(%s) = version %d, average %v, %d of %d documents, want 3, 2.5, 1 of 1", file, index.Version, index.AvgBodyLen, len(index.Docs), index.DocCount)
	}
	// A term listed twice keeps its later count, as it does in a map.
	if got := index.DF["run"]; got != 9 || len(index.DF) != 2 {
		t.Errorf("ReadAskIndex(%s).DF = %v, want run: 9 and a: 1", file, index.DF)
	}
	doc := index.Docs["a.go#Run"]
	for _, tt := range []struct {
		bag       string
		counts    AskCounts
		term      string
		want      int
		wantThere bool
	}{
		{bag: "name", counts: doc.Name, term: "run", want: 1, wantThere: true},
		{bag: "path", counts: doc.Path, term: "go", want: 1, wantThere: true},
		{bag: "path", counts: doc.Path, term: "run"},
		{bag: "body", counts: doc.Body, term: "run", want: 3, wantThere: true},
		// A term the file lists with a count of zero is still listed.
		{bag: "body", counts: doc.Body, term: "fast", wantThere: true},
		{bag: "body", counts: doc.Body, term: "slow"},
	} {
		if got, there := tt.counts.count(tt.term); got != tt.want || there != tt.wantThere {
			t.Errorf("%s bag count(%q) = %d, %v, want %d, %v", tt.bag, tt.term, got, there, tt.want, tt.wantThere)
		}
	}
	if got := askBagLength(doc.Body); got != 3 {
		t.Errorf("askBagLength(body) = %d, want 3", got)
	}
}
