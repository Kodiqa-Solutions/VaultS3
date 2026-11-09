package s3

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func selectBody(compression string) string {
	return `<SelectObjectContentRequest><Expression>SELECT * FROM S3Object</Expression><ExpressionType>SQL</ExpressionType>` +
		`<InputSerialization><CompressionType>` + compression + `</CompressionType><CSV><FileHeaderInfo>NONE</FileHeaderInfo></CSV></InputSerialization>` +
		`<OutputSerialization><CSV/></OutputSerialization></SelectObjectContentRequest>`
}

// Regression (5.0.14 security review): only the stored size was limited, so
// a 400 KB gzip of a 400 MB CSV took 2 GB of heap in one Select request.
// The decompressed input, the fields held and the requests running at once
// are bounded now.
func TestSelectInputIsBoundedAfterDecompression(t *testing.T) {
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for i := 0; i < 400; i++ {
		_, _ = zw.Write(chunk)
	}
	_ = zw.Close()
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/selb", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/selb/x.csv", gz.String()).must(t, "put")

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak uint64
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	r := e.admin(t, http.MethodPost, "/selb/x.csv?select&select-type=2", selectBody("GZIP"))
	close(done)
	<-stopped
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "OverMaxRecordSize") {
		t.Fatalf("a 400 MB decompressed select: %d %.200s", r.code, r.body)
	}
	if grown := int64(peak) - int64(before.HeapAlloc); grown > 512<<20 {
		t.Errorf("the refused select still grew the heap by %d MB", grown>>20)
	}
}

// A CSV within the byte limit but with too many fields is refused before it
// is all held.
func TestSelectFieldsAreBounded(t *testing.T) {
	prev := maxSelectCells
	maxSelectCells = 1000
	defer func() { maxSelectCells = prev }()
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/selb", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/selb/wide.csv", strings.Repeat(strings.Repeat("a,", 99)+"a\n", 20)).must(t, "put")
	r := e.admin(t, http.MethodPost, "/selb/wide.csv?select&select-type=2", selectBody("NONE"))
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "OverMaxRecordSize") {
		t.Fatalf("2000 fields over a limit of 1000: %d %.200s", r.code, r.body)
	}
	e.admin(t, http.MethodPut, "/selb/small.csv", "a,b\nc,d\n").must(t, "put")
	if r := e.admin(t, http.MethodPost, "/selb/small.csv?select&select-type=2", selectBody("NONE")); r.code != http.StatusOK {
		t.Fatalf("a small select: %d", r.code)
	}
}

// R9-5: Parquet input is bounded by file bytes and by cell count, not by the
// bytes the cells decode to. A dictionary-encoded column repeating one large
// value is a few KB on disk, and every row gets its own copy of the value.
func TestSelectParquetDictionaryIsBounded(t *testing.T) {
	type row struct {
		S string `parquet:"s,dict,zstd"`
	}
	big := strings.Repeat("A", 1<<20) // 1 MiB value
	rows := make([]row, 300)
	for i := range rows {
		rows[i].S = big
	}
	var buf bytes.Buffer
	if err := parquet.Write(&buf, rows); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	recs, err := parseParquetInput(&selectLimit{r: bytes.NewReader(buf.Bytes()), n: maxSelectInput})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errSelectTooLarge) {
		t.Errorf("a Parquet column decoding to 300 MiB was accepted: %v", err)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("file %d bytes -> %d records, %d MiB allocated", buf.Len(), len(recs), alloc>>20)
	// What one request may hold is the value budget; decoding and formatting
	// allocate it about twice over, garbage included.
	if alloc > uint64(3*maxSelectValueBytes) {
		t.Errorf("a %d-byte Parquet file made one Select allocate %d MiB (budget %d MiB)", buf.Len(), alloc>>20, maxSelectValueBytes>>20)
	}
}

// R9-5b: JSON cells are counted at the top level only; one record holding a
// nested array of small numbers decodes to many times its size.
func TestSelectNestedJSONIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"a":[`)
	n := 30 << 20 / 2
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('1')
	}
	b.WriteString(`]}`)
	in := b.String()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := parseJSONInput(&selectLimit{r: strings.NewReader(in), n: maxSelectInput}, &jsonInput{Type: "LINES"})
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("input %d MiB, err %v, %d MiB allocated (%dx)", len(in)>>20, err, alloc>>20, alloc/uint64(len(in)))
	if alloc > uint64(8*len(in)) {
		t.Errorf("one 1-cell JSON record of %d MiB allocated %d MiB", len(in)>>20, alloc>>20)
	}
}

// The same nested record as a JSON document is refused before it is decoded.
func TestSelectNestedJSONDocumentIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{"a":[`)
	for i := 0; i < 4<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('1')
	}
	b.WriteString(`]}]`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := parseJSONInput(&selectLimit{r: strings.NewReader(b.String()), n: maxSelectInput}, &jsonInput{Type: "DOCUMENT"})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errSelectTooLarge) {
		t.Errorf("an 8 MiB record was accepted: %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("an 8 MiB document allocated %d MiB", alloc>>20)
	}
}

// The streaming DOCUMENT reader returns what decoding the whole document did.
func TestSelectJSONDocumentForms(t *testing.T) {
	for in, want := range map[string][]string{
		`[{"a":1},{"a":"x]}\"y"}]`:            {"1", `x]}"y`},
		` {"a":{"b":[1,2]}} `:                 {"map[b:[1 2]]"},
		`{"a":1} {"a":2}`:                     {"1", "2"},
		"[ ]":                                 nil,
		"[\n {\"a\":\"[{\"} ,\n {\"a\":3}\n]": {"[{", "3"},
	} {
		recs, err := parseJSONInput(strings.NewReader(in), &jsonInput{Type: "DOCUMENT"})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		var got []string
		for _, r := range recs {
			got = append(got, r["a"])
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{`[{"a":1}`, `[{"a":1} {"a":2}]`, `{"a":`, `[1]`} {
		if _, err := parseJSONInput(strings.NewReader(bad), &jsonInput{Type: "DOCUMENT"}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
