package cwmp

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Auth checks the credentials a device sends with the first message of a
// session (TR-069 §3.4.4): HTTP Basic or Digest (MD5, qop=auth or none). One
// shared username and password, as set on the ONTs' ManagementServer.Username
// and ManagementServer.Password by whatever provisions the ACS URL.
type Auth struct {
	Username string
	Password string
	Realm    string

	key      []byte        // signs nonces, so no nonce table is needed
	nonceTTL time.Duration // how long a challenge stays usable
}

// NewAuth builds an Auth with a fresh nonce key.
func NewAuth(username, password string) *Auth {
	key := make([]byte, 32)
	rand.Read(key)
	return &Auth{Username: username, Password: password, Realm: serverName, key: key, nonceTTL: 5 * time.Minute}
}

// Challenge offers Digest first, then Basic; the device picks.
func (a *Auth) Challenge(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Digest realm="`+a.Realm+`", qop="auth", algorithm=MD5, nonce="`+a.nonce(time.Now())+`"`)
	w.Header().Add("WWW-Authenticate", `Basic realm="`+a.Realm+`"`)
}

// Check reports whether r carries valid credentials.
func (a *Auth) Check(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	scheme, rest, _ := strings.Cut(h, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		u, p, ok := r.BasicAuth()
		return ok && eq(u, a.Username) && eq(p, a.Password)
	case "digest":
		return a.checkDigest(r.Method, digestParams(rest))
	}
	return false
}

func (a *Auth) checkDigest(method string, p map[string]string) bool {
	if !eq(p["username"], a.Username) || p["realm"] != a.Realm || !a.validNonce(p["nonce"], time.Now()) {
		return false
	}
	if alg := strings.ToUpper(p["algorithm"]); alg != "" && alg != "MD5" {
		return false
	}
	ha1 := md5hex(a.Username + ":" + a.Realm + ":" + a.Password)
	ha2 := md5hex(method + ":" + p["uri"])
	var want string
	switch p["qop"] {
	case "auth":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	case "":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	default:
		return false
	}
	return eq(strings.ToLower(p["response"]), want)
}

// nonce is "<unix seconds>.<hmac>", so it can be checked without state.
func (a *Auth) nonce(now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	return ts + "." + a.sign(ts)
}

func (a *Auth) validNonce(n string, now time.Time) bool {
	ts, sig, ok := strings.Cut(n, ".")
	if !ok || !eq(sig, a.sign(ts)) {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(sec, 0))
	return age >= -time.Minute && age <= a.nonceTTL
}

func (a *Auth) sign(s string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// digestParams parses `k=v, k="v, with comma"` pairs.
func digestParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		rest = strings.TrimSpace(rest)
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				break
			}
			v, rest = rest[1:end+1], rest[end+2:]
		} else {
			v, rest, _ = strings.Cut(rest, ",")
			v = strings.TrimSpace(v)
			rest = "," + rest
		}
		out[k] = v
		_, rest, _ = strings.Cut(rest, ",")
		s = strings.TrimSpace(rest)
	}
	return out
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
