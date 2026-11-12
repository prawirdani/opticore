// Package cwmp implements a zero-copy XML scanner and SOAP 1.1 codec for
// CWMP (TR-069).
package cwmp

import (
	"bytes"
	"errors"
	"unicode/utf8"
)

// maxDepth bounds element nesting. CWMP messages nest well below this.
const maxDepth = 64

const maxInt32 = 1<<31 - 1

var (
	errDTD          = errors.New("cwmp/xml: DTD not supported")
	errMalformed    = errors.New("cwmp/xml: malformed tag")
	errUnclosed     = errors.New("cwmp/xml: unclosed element")
	errMismatch     = errors.New("cwmp/xml: mismatched end tag")
	errTooDeep      = errors.New("cwmp/xml: nesting too deep")
	errTooMany      = errors.New("cwmp/xml: too many nodes")
	errUnterminated = errors.New("cwmp/xml: unterminated comment, PI or CDATA")
	errBadEntity    = errors.New("cwmp/xml: bad entity")
	errCDATAOut     = errors.New("cwmp/xml: CDATA outside root element")
)

const attrChunkSize = 256

var (
	commentStart = []byte("<!--")
	commentEnd   = []byte("-->")
	cdataStart   = []byte("<![CDATA[")
	cdataEnd     = []byte("]]>")
)

// Attr is one attribute. Prefix is nil when the attribute is unprefixed.
// Local and Value are sub-slices of the parsed input unless Value contained an
// entity reference.
type Attr struct {
	Prefix []byte
	Local  []byte
	Value  []byte
}

// Node is one element. Indices are into Doc.Nodes; -1 means none.
// Text is the concatenated, entity-decoded, whitespace-trimmed character data
// directly inside the element.
type Node struct {
	Prefix      []byte
	Local       []byte
	Attrs       []Attr
	Text        []byte
	Parent      int32
	FirstChild  int32
	NextSibling int32
}

// Doc is a reusable XML document. Parse invalidates previous contents.
type Doc struct {
	Nodes []Node

	stack []int32
	last  []int32
	tmp   []Attr

	attrChunks [][]Attr
	attrChunk  int
	attrUsed   int
}

// Reset drops the parsed nodes but keeps their memory for the next Parse.
func (d *Doc) Reset() {
	d.Nodes = d.Nodes[:0]
	d.stack = d.stack[:0]
	d.last = d.last[:0]
	d.tmp = d.tmp[:0]
	d.attrChunk = 0
	d.attrUsed = 0
}

// Child returns the first direct child of i whose local name matches, or -1.
func (d *Doc) Child(i int32, local string) int32 {
	if i < 0 || i >= int32(len(d.Nodes)) {
		return -1
	}
	for c := d.Nodes[i].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
		if localIs(&d.Nodes[c], local) {
			return c
		}
	}
	return -1
}

// Attr returns the value of the attribute with that local name, ignoring its
// prefix. Namespace declarations are skipped.
func (d *Doc) Attr(i int32, local string) ([]byte, bool) {
	if i < 0 || i >= int32(len(d.Nodes)) {
		return nil, false
	}
	for k := range d.Nodes[i].Attrs {
		a := &d.Nodes[i].Attrs[k]
		if a.Prefix != nil && string(a.Prefix) == "xmlns" {
			continue
		}
		if string(a.Local) == local {
			return a.Value, true
		}
	}
	return nil, false
}

// NamespaceURI resolves prefix as bound on i or any ancestor. A nil prefix
// resolves the default namespace. It returns nil when unbound.
func (d *Doc) NamespaceURI(i int32, prefix []byte) []byte {
	for i >= 0 {
		nd := &d.Nodes[i]
		for k := range nd.Attrs {
			a := &nd.Attrs[k]
			if a.Prefix != nil {
				if bytes.Equal(a.Prefix, xmlnsPrefix) && bytes.Equal(a.Local, prefix) {
					return a.Value
				}
			} else if len(prefix) == 0 && bytes.Equal(a.Local, xmlnsLocal) {
				return a.Value
			}
		}
		i = nd.Parent
	}
	if bytes.Equal(prefix, xmlPrefix) {
		return xmlNamespace
	}
	return nil
}

// Descendant returns the first descendant of i (depth first) with that local
// name, or -1.
func (d *Doc) Descendant(i int32, local string) int32 {
	if i < 0 {
		return -1
	}
	for c := d.Nodes[i].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
		if localIs(&d.Nodes[c], local) {
			return c
		}
		if r := d.Descendant(c, local); r >= 0 {
			return r
		}
	}
	return -1
}

var (
	xmlnsPrefix  = []byte("xmlns")
	xmlnsLocal   = []byte("xmlns")
	xmlPrefix    = []byte("xml")
	xmlNamespace = []byte("http://www.w3.org/XML/1998/namespace")
)

// Parse scans b into d. It never panics; malformed input returns an error.
// Text and attribute values are sub-slices of b unless they contain entities.
func (d *Doc) Parse(b []byte) error {
	d.Reset()
	i := 0
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		i = 3
	}
	for {
		lt := bytes.IndexByte(b[i:], '<')
		if lt < 0 {
			rest := b[i:]
			if len(d.stack) > 0 {
				if err := d.addText(d.stack[len(d.stack)-1], rest, true); err != nil {
					return err
				}
				return errUnclosed
			}
			break
		}
		lt += i
		if lt > i && len(d.stack) > 0 {
			if err := d.addText(d.stack[len(d.stack)-1], b[i:lt], true); err != nil {
				return err
			}
		}
		i = lt
		rest := b[i:]
		if len(rest) < 2 {
			return errMalformed
		}
		switch rest[1] {
		case '!':
			switch {
			case bytes.HasPrefix(rest, commentStart):
				j := bytes.Index(rest[len(commentStart):], commentEnd)
				if j < 0 {
					return errUnterminated
				}
				i += len(commentStart) + j + len(commentEnd)
			case bytes.HasPrefix(rest, cdataStart):
				if len(d.stack) == 0 {
					return errCDATAOut
				}
				j := bytes.Index(rest[len(cdataStart):], cdataEnd)
				if j < 0 {
					return errUnterminated
				}
				if err := d.addText(d.stack[len(d.stack)-1], rest[len(cdataStart):len(cdataStart)+j], false); err != nil {
					return err
				}
				i += len(cdataStart) + j + len(cdataEnd)
			case bytes.HasPrefix(rest, []byte("<!DOCTYPE")):
				return errDTD
			default:
				// Other directives are not part of XML but encoding/xml
				// tolerates them; skip to the closing '>' for parity.
				j := bytes.IndexByte(rest, '>')
				if j < 0 {
					return errUnterminated
				}
				i += j + 1
			}
		case '?':
			j := bytes.Index(rest[2:], []byte("?>"))
			if j < 0 {
				return errUnterminated
			}
			i += 2 + j + 2
		case '/':
			var err error
			i, err = d.endTag(b, i)
			if err != nil {
				return err
			}
		default:
			var err error
			i, _, err = d.startTag(b, i)
			if err != nil {
				return err
			}
		}
	}
	if len(d.stack) != 0 {
		return errUnclosed
	}
	return nil
}

func (d *Doc) addText(n int32, seg []byte, decode bool) error {
	if len(seg) == 0 {
		return nil
	}
	nd := &d.Nodes[n]
	if decode && bytes.IndexByte(seg, '&') >= 0 {
		dec, err := decodeEntities(seg)
		if err != nil {
			return err
		}
		if nd.Text == nil {
			nd.Text = dec
		} else {
			nd.Text = appendCopy(nd.Text, dec)
		}
		return nil
	}
	if nd.Text == nil {
		// Leading whitespace-only runs trim away; skipping them avoids a copy
		// when a later run arrives.
		if isAllSpace(seg) {
			return nil
		}
		nd.Text = seg
		return nil
	}
	nd.Text = appendCopy(nd.Text, seg)
	return nil
}

func (d *Doc) startTag(b []byte, i int) (int, bool, error) {
	i++ // '<'
	ns := i
	for i < len(b) && !isSpace(b[i]) && b[i] != '/' && b[i] != '>' && b[i] != '=' && b[i] != '<' {
		i++
	}
	if i == ns || i >= len(b) || b[i] == '<' {
		return i, false, errMalformed
	}
	prefix, local := splitName(b[ns:i])

	d.tmp = d.tmp[:0]
	for {
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		if i >= len(b) {
			return i, false, errUnclosed
		}
		switch b[i] {
		case '>':
			i++
			if _, err := d.addNode(prefix, local, false); err != nil {
				return i, false, err
			}
			return i, false, nil
		case '/':
			if i+1 < len(b) && b[i+1] == '>' {
				i += 2
				if _, err := d.addNode(prefix, local, true); err != nil {
					return i, false, err
				}
				return i, true, nil
			}
			return i, false, errMalformed
		}
		as := i
		for i < len(b) && !isSpace(b[i]) && b[i] != '=' && b[i] != '>' && b[i] != '/' && b[i] != '<' {
			i++
		}
		if i == as {
			return i, false, errMalformed
		}
		aname := b[as:i]
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		if i >= len(b) || b[i] != '=' {
			return i, false, errMalformed
		}
		i++
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		if i >= len(b) || (b[i] != '"' && b[i] != '\'') {
			return i, false, errMalformed
		}
		q := b[i]
		i++
		vs := i
		for i < len(b) && b[i] != q {
			i++
		}
		if i >= len(b) {
			return i, false, errUnclosed
		}
		val := b[vs:i]
		i++
		if bytes.IndexByte(val, '&') >= 0 {
			dec, err := decodeEntities(val)
			if err != nil {
				return i, false, err
			}
			val = dec
		}
		ap, al := splitName(aname)
		d.tmp = append(d.tmp, Attr{Prefix: ap, Local: al, Value: val})
	}
}

func (d *Doc) addNode(prefix, local []byte, selfClose bool) (int32, error) {
	parent := int32(-1)
	if n := len(d.stack); n > 0 {
		parent = d.stack[n-1]
	}
	if len(d.Nodes) >= maxInt32 {
		return 0, errTooMany
	}
	if !selfClose && len(d.stack) >= maxDepth {
		return 0, errTooDeep
	}
	idx := int32(len(d.Nodes))
	var attrs []Attr
	if n := len(d.tmp); n > 0 {
		attrs = d.allocAttrs(n)
		copy(attrs, d.tmp)
	}
	d.Nodes = append(d.Nodes, Node{
		Prefix:      prefix,
		Local:       local,
		Attrs:       attrs,
		Parent:      parent,
		FirstChild:  -1,
		NextSibling: -1,
	})
	d.last = append(d.last, -1)
	if parent >= 0 {
		if last := d.last[parent]; last < 0 {
			d.Nodes[parent].FirstChild = idx
		} else {
			d.Nodes[last].NextSibling = idx
		}
		d.last[parent] = idx
	}
	if !selfClose {
		d.stack = append(d.stack, idx)
	}
	return idx, nil
}

func (d *Doc) allocAttrs(n int) []Attr {
	for {
		if d.attrChunk < len(d.attrChunks) {
			c := d.attrChunks[d.attrChunk]
			if d.attrUsed+n <= len(c) {
				s := c[d.attrUsed : d.attrUsed+n]
				d.attrUsed += n
				return s
			}
			d.attrChunk++
			d.attrUsed = 0
			continue
		}
		size := attrChunkSize
		if n > size {
			size = n
		}
		c := make([]Attr, size)
		d.attrChunks = append(d.attrChunks, c)
		d.attrChunk = len(d.attrChunks) - 1
		d.attrUsed = n
		return c[:n]
	}
}

func (d *Doc) endTag(b []byte, i int) (int, error) {
	i += 2 // "</"
	ns := i
	for i < len(b) && !isSpace(b[i]) && b[i] != '>' {
		i++
	}
	name := b[ns:i]
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '>' {
		return i, errMalformed
	}
	i++
	if len(d.stack) == 0 {
		return i, errMismatch
	}
	idx := d.stack[len(d.stack)-1]
	nd := &d.Nodes[idx]
	if !qnameEq(name, nd.Prefix, nd.Local) {
		return i, errMismatch
	}
	nd.Text = trimSpace(nd.Text)
	d.stack = d.stack[:len(d.stack)-1]
	return i, nil
}

func splitName(b []byte) (prefix, local []byte) {
	if i := bytes.IndexByte(b, ':'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return nil, b
}

func qnameEq(name, prefix, local []byte) bool {
	if len(prefix) == 0 {
		return bytes.Equal(name, local)
	}
	return len(name) == len(prefix)+1+len(local) &&
		bytes.Equal(name[:len(prefix)], prefix) &&
		name[len(prefix)] == ':' &&
		bytes.Equal(name[len(prefix)+1:], local)
}

func localIs(n *Node, local string) bool {
	return string(n.Local) == local
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isAllSpace(b []byte) bool {
	for _, c := range b {
		if !isSpace(c) {
			return false
		}
	}
	return true
}

func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	j := len(b)
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func appendCopy(dst, src []byte) []byte {
	out := make([]byte, 0, len(dst)+len(src))
	out = append(out, dst...)
	return append(out, src...)
}

// decodeEntities expands the five XML entities and numeric character
// references. It allocates; callers only use it when '&' is present.
func decodeEntities(s []byte) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '&' {
			out = append(out, c)
			i++
			continue
		}
		semi := bytes.IndexByte(s[i:], ';')
		if semi < 0 {
			return nil, errBadEntity
		}
		ent := s[i+1 : i+semi]
		i += semi + 1
		switch len(ent) {
		case 2:
			if string(ent) == "lt" {
				out = append(out, '<')
				continue
			}
			if string(ent) == "gt" {
				out = append(out, '>')
				continue
			}
		case 3:
			if string(ent) == "amp" {
				out = append(out, '&')
				continue
			}
		case 4:
			if string(ent) == "quot" {
				out = append(out, '"')
				continue
			}
			if string(ent) == "apos" {
				out = append(out, '\'')
				continue
			}
		}
		if len(ent) < 2 || ent[0] != '#' {
			return nil, errBadEntity
		}
		hex := ent[1] == 'x' || ent[1] == 'X'
		digits := ent[1:]
		if hex {
			digits = ent[2:]
		}
		if len(digits) == 0 {
			return nil, errBadEntity
		}
		var r rune
		for _, d := range digits {
			if hex {
				var v byte
				switch {
				case d >= '0' && d <= '9':
					v = d - '0'
				case d >= 'a' && d <= 'f':
					v = d - 'a' + 10
				case d >= 'A' && d <= 'F':
					v = d - 'A' + 10
				default:
					return nil, errBadEntity
				}
				r = r*16 + rune(v)
			} else {
				if d < '0' || d > '9' {
					return nil, errBadEntity
				}
				r = r*10 + rune(d-'0')
			}
			if r > utf8.MaxRune {
				return nil, errBadEntity
			}
		}
		if r == 0 || (r >= 0xD800 && r <= 0xDFFF) || r > utf8.MaxRune {
			return nil, errBadEntity
		}
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}
