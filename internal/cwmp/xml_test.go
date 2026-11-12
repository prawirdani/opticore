package cwmp

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"strings"
	"testing"
	"unsafe"
)

func TestParseShape(t *testing.T) {
	b := []byte(`<?xml version="1.0"?>
<!-- top -->
<soap-env:Envelope xmlns:soap-env="urn:env" xmlns:cwmp="urn:cwmp">
  <soap-env:Header><cwmp:ID must="1">7</cwmp:ID></soap-env:Header>
  <soap-env:Body>
    <cwmp:Inform>
      <cwmp:DeviceID><cwmp:OUI>00259E</cwmp:OUI></cwmp:DeviceID>
      <cwmp:MaxEnvelopes>1</cwmp:MaxEnvelopes>
    </cwmp:Inform>
  </soap-env:Body>
</soap-env:Envelope>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	root := int32(0)
	if !localIs(&d.Nodes[root], "Envelope") || string(d.Nodes[root].Prefix) != "soap-env" {
		t.Fatalf("root = %s:%s", d.Nodes[root].Prefix, d.Nodes[root].Local)
	}
	body := d.Child(root, "Body")
	if body < 0 {
		t.Fatal("no Body")
	}
	inform := d.Child(body, "Inform")
	if inform < 0 {
		t.Fatal("no Inform")
	}
	if max := d.Child(inform, "MaxEnvelopes"); max < 0 || string(d.Nodes[max].Text) != "1" {
		t.Fatal("MaxEnvelopes not parsed")
	}
	did := d.Child(inform, "DeviceID")
	if did < 0 {
		t.Fatal("no DeviceID")
	}
	if oui := d.Child(did, "OUI"); oui < 0 || string(d.Nodes[oui].Text) != "00259E" {
		t.Fatal("OUI not parsed")
	}
	if uri := d.NamespaceURI(inform, []byte("cwmp")); string(uri) != "urn:cwmp" {
		t.Fatalf("cwmp ns = %q", uri)
	}
}

func TestParseTextIsSubslice(t *testing.T) {
	b := []byte(`<r><v>hello</v></r>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	v := d.Child(0, "v")
	if v < 0 {
		t.Fatal("no v")
	}
	text := d.Nodes[v].Text
	if string(text) != "hello" {
		t.Fatalf("text = %q", text)
	}
	lo := uintptr(unsafe.Pointer(&b[0]))
	hi := lo + uintptr(len(b))
	p := uintptr(unsafe.Pointer(unsafe.SliceData(text)))
	if p < lo || p+uintptr(len(text)) > hi {
		t.Fatal("text was copied instead of sub-sliced")
	}
}

func TestParseEntities(t *testing.T) {
	b := []byte(`<r a="x &amp; y &lt;z&gt; &#65;&#x42;&apos;&quot;">t &amp; &#9731;&#x1F600;</r>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	if got, want := string(d.Nodes[0].Text), "t & ☃😀"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	av, ok := d.Attr(0, "a")
	if !ok {
		t.Fatal("no attr a")
	}
	if got, want := string(av), `x & y <z> AB'"`; got != want {
		t.Fatalf("attr = %q, want %q", got, want)
	}
}

func TestParseCDATA(t *testing.T) {
	b := []byte(`<r><![CDATA[a < b & c]]>tail</r>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	if got, want := string(d.Nodes[0].Text), "a < b & ctail"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestParseCommentInsideText(t *testing.T) {
	b := []byte("<r>a<!-- x -->b<?pi data?>c</r>")
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	if got := string(d.Nodes[0].Text); got != "abc" {
		t.Fatalf("text = %q", got)
	}
}

func TestParseTrimsWhitespace(t *testing.T) {
	b := []byte("<r>\n  <v> x </v>\n  <w>  </w>\n</r>")
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	v := d.Child(0, "v")
	w := d.Child(0, "w")
	if got := string(d.Nodes[v].Text); got != "x" {
		t.Fatalf("v text = %q", got)
	}
	if got := d.Nodes[w].Text; got != nil {
		t.Fatalf("w text = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	deep := strings.Repeat("<a>", 70) + strings.Repeat("</a>", 70)
	cases := []struct{ name, in string }{
		{"doctype", `<!DOCTYPE r SYSTEM "x"><r/>`},
		{"doctype-upper", `<!DOCTYPE r><r/>`},
		{"unclosed", `<r><a></a>`},
		{"mismatch", `<r><a></b></r>`},
		{"stray-close", `<r></r></r>`},
		{"bad-entity", `<r>a &bogus; b</r>`},
		{"unquoted-attr", `<r a=b/>`},
		{"unterminated-comment", `<r><!-- x </r>`},
		{"unterminated-cdata", `<r><![CDATA[x</r>`},
		{"too-deep", deep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d Doc
			if err := d.Parse([]byte(tc.in)); err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", tc.in)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	var d Doc
	if err := d.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if len(d.Nodes) != 0 {
		t.Fatalf("got %d nodes", len(d.Nodes))
	}
}

func TestDocResetReusesCapacity(t *testing.T) {
	b := []byte(`<r a="1"><b x="2" y="3">t</b></r>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	nodes := cap(d.Nodes)
	chunks := len(d.attrChunks)
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	if cap(d.Nodes) != nodes || len(d.attrChunks) != chunks {
		t.Fatalf("capacity not reused: nodes %d->%d chunks %d->%d", nodes, cap(d.Nodes), chunks, len(d.attrChunks))
	}
	if len(d.Nodes) != 2 || len(d.Nodes[1].Attrs) != 2 {
		t.Fatalf("bad tree after reuse: %+v", d.Nodes)
	}
}

func TestAttrArenaReuseDoesNotGrow(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<r>")
	for i := 0; i < 300; i++ {
		sb.WriteString(`<e a="1" b="2"/>`)
	}
	sb.WriteString("</r>")
	b := []byte(sb.String())
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	chunks := len(d.attrChunks)
	if chunks < 2 {
		t.Fatalf("expected multiple chunks, got %d", chunks)
	}
	for i := 0; i < 3; i++ {
		if err := d.Parse(b); err != nil {
			t.Fatal(err)
		}
		if len(d.attrChunks) != chunks {
			t.Fatalf("chunk count grew on parse %d: %d -> %d", i, chunks, len(d.attrChunks))
		}
	}
	if len(d.Nodes) != 301 {
		t.Fatalf("nodes = %d", len(d.Nodes))
	}
}

func TestNamespaceDefaultAndPrefix(t *testing.T) {
	b := []byte(`<a xmlns="urn:default" xmlns:p="urn:p"><p:b/></a>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	child := d.Child(0, "b")
	if got := d.NamespaceURI(0, nil); string(got) != "urn:default" {
		t.Fatalf("default ns = %q", got)
	}
	if got := d.NamespaceURI(child, []byte("p")); string(got) != "urn:p" {
		t.Fatalf("p ns = %q", got)
	}
	if got := d.NamespaceURI(child, []byte("q")); got != nil {
		t.Fatalf("q ns = %q, want nil", got)
	}
}

func TestNodeSiblingLinks(t *testing.T) {
	b := []byte(`<r><a/><b/><c/></r>`)
	var d Doc
	if err := d.Parse(b); err != nil {
		t.Fatal(err)
	}
	var got []string
	for c := d.Nodes[0].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
		got = append(got, string(d.Nodes[c].Local))
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("children = %v", got)
	}
	for _, c := range []int32{1, 2, 3} {
		if d.Nodes[c].Parent != 0 {
			t.Fatalf("node %d parent = %d", c, d.Nodes[c].Parent)
		}
	}
}

// --- differential fuzz against encoding/xml ---

type xmlEl struct {
	local string
	text  string
}

func normText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func stdlibElements(b []byte) ([]xmlEl, bool) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	var out []xmlEl
	var stack []int
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			out = append(out, xmlEl{local: t.Name.Local})
			stack = append(stack, len(out)-1)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, false
			}
			idx := stack[len(stack)-1]
			out[idx].text = normText(out[idx].text)
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				idx := stack[len(stack)-1]
				out[idx].text += string(t)
			}
		}
	}
	if len(stack) != 0 {
		return nil, false
	}
	return out, true
}

func ourElements(b []byte) ([]xmlEl, error) {
	var d Doc
	if err := d.Parse(b); err != nil {
		return nil, err
	}
	out := make([]xmlEl, len(d.Nodes))
	for i := range d.Nodes {
		out[i] = xmlEl{local: string(d.Nodes[i].Local), text: normText(string(d.Nodes[i].Text))}
	}
	return out, nil
}

func FuzzScanner(f *testing.F) {
	ents, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		b, err := os.ReadFile("testdata/" + e.Name())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if bytes.Contains(b, []byte("<!DOCTYPE")) {
			return // no DTD support by design
		}
		want, ok := stdlibElements(b)
		if !ok {
			return
		}
		got, err := ourElements(b)
		if err != nil {
			t.Fatalf("encoding/xml accepted %d elements, scanner failed: %v; input %q", len(want), err, b)
		}
		if len(got) != len(want) {
			t.Fatalf("element count %d != %d; input %q", len(got), len(want), b)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("element %d = %+v, want %+v; input %q", i, got[i], want[i], b)
			}
		}
	})
}
