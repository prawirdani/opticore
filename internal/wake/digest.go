package wake

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// digestChallenge is the per-device cache of what the device asked for.
type digestChallenge struct {
	Realm     string
	Nonce     string
	Opaque    string
	Algorithm string
	QOP       string
	Stale     bool
	NC        uint32
}

// authorization builds the Authorization header value for one attempt. The
// caller owns the NC counter and must increment it before each use.
// qop/nc are unquoted per RFC 2617; the other fields are quoted.
func (c *digestChallenge) authorization(method, uri, username, password string) string {
	nc := fmt.Sprintf("%08x", c.NC)
	cnonce := newCnonce()

	ha1 := md5hex(username + ":" + c.Realm + ":" + password)
	if strings.EqualFold(c.Algorithm, "MD5-sess") {
		ha1 = md5hex(ha1 + ":" + c.Nonce + ":" + cnonce)
	}
	ha2 := md5hex(method + ":" + uri)

	var response string
	if c.QOP != "" {
		response = md5hex(ha1 + ":" + c.Nonce + ":" + nc + ":" + cnonce + ":" + c.QOP + ":" + ha2)
	} else {
		response = md5hex(ha1 + ":" + c.Nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Digest username=%s, realm=%s, nonce=%s, uri=%s, response=%s",
		quote(username), quote(c.Realm), quote(c.Nonce), quote(uri), quote(response))
	if c.Algorithm != "" {
		fmt.Fprintf(&b, ", algorithm=%s", c.Algorithm)
	}
	if c.Opaque != "" {
		fmt.Fprintf(&b, ", opaque=%s", quote(c.Opaque))
	}
	if c.QOP != "" {
		fmt.Fprintf(&b, ", qop=%s, nc=%s, cnonce=%s", c.QOP, nc, quote(cnonce))
	}
	return b.String()
}

// parseChallenges picks the first usable Digest challenge and notes whether a
// Basic challenge was offered. Multiple headers and comma-joined challenges
// are handled; unsupported Digest algorithms are skipped.
func parseChallenges(headers []string) (digest *digestChallenge, basic bool) {
	for _, header := range headers {
		scheme := ""
		params := map[string]string{}
		flush := func() {
			switch scheme {
			case "basic":
				basic = true
			case "digest":
				if digest == nil {
					digest = newDigest(params)
				}
			}
		}
		for _, item := range authItems(header) {
			low := strings.ToLower(item)
			switch {
			case item == "":
				continue
			case low == "digest" || strings.HasPrefix(low, "digest "):
				flush()
				scheme, params = "digest", map[string]string{}
				item = strings.TrimSpace(item[len("digest"):])
				if item == "" {
					continue
				}
			case low == "basic" || strings.HasPrefix(low, "basic "):
				flush()
				scheme, params = "basic", map[string]string{}
				item = strings.TrimSpace(item[len("basic"):])
				if item == "" {
					continue
				}
			}
			if k, v, ok := cutParam(item); ok {
				params[k] = v
			}
		}
		flush()
	}
	return digest, basic
}

func newDigest(params map[string]string) *digestChallenge {
	nonce := params["nonce"]
	if nonce == "" {
		return nil
	}
	alg := params["algorithm"]
	switch {
	case alg == "":
		alg = "MD5"
	case strings.EqualFold(alg, "MD5"):
		alg = "MD5"
	case strings.EqualFold(alg, "MD5-sess"):
		alg = "MD5-sess"
	default:
		return nil
	}
	qop := ""
	if raw := params["qop"]; raw != "" {
		for _, q := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(q), "auth") {
				qop = "auth"
				break
			}
		}
		if qop == "" {
			return nil
		}
	}
	return &digestChallenge{
		Realm:     params["realm"],
		Nonce:     nonce,
		Opaque:    params["opaque"],
		Algorithm: alg,
		QOP:       qop,
		Stale:     strings.EqualFold(params["stale"], "true"),
	}
}

// authItems splits a challenge on commas that are outside quoted strings.
func authItems(h string) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(h); i++ {
		switch h[i] {
		case '\\':
			if inQuote {
				i++
			}
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, strings.TrimSpace(h[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(h[start:]))
}

// cutParam splits key=value and unquotes the value.
func cutParam(s string) (key, val string, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(s[:i]))
	val = strings.TrimSpace(s[i+1:])
	if val == "" || val[0] != '"' {
		return key, val, true
	}
	var b strings.Builder
	for j := 1; j < len(val); j++ {
		switch val[j] {
		case '\\':
			if j+1 < len(val) {
				j++
				b.WriteByte(val[j])
			}
		case '"':
			return key, b.String(), true
		default:
			b.WriteByte(val[j])
		}
	}
	return key, b.String(), true
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newCnonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
