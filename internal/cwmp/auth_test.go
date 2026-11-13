package cwmp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func digestHeader(a *Auth, user, pass, nonce, uri string) string {
	ha1 := md5hex(user + ":" + a.Realm + ":" + pass)
	ha2 := md5hex("POST:" + uri)
	resp := md5hex(ha1 + ":" + nonce + ":00000001:abc:auth:" + ha2)
	return `Digest username="` + user + `", realm="` + a.Realm + `", nonce="` + nonce +
		`", uri="` + uri + `", qop=auth, nc=00000001, cnonce="abc", response="` + resp + `", algorithm=MD5`
}

func TestAuthBasicAndDigest(t *testing.T) {
	a := NewAuth("acs", "pw, with comma")
	req := func(h string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/acs", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}
	basic := func(u, p string) string {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.SetBasicAuth(u, p)
		return r.Header.Get("Authorization")
	}
	n := a.nonce(time.Now())
	stale := a.nonce(time.Now().Add(-time.Hour))
	forged := "9999999999." + strings.Repeat("A", 22)
	for _, tc := range []struct {
		name, h string
		want    bool
	}{
		{"none", "", false},
		{"basic ok", basic("acs", "pw, with comma"), true},
		{"basic wrong", basic("acs", "nope"), false},
		{"digest ok", digestHeader(a, "acs", "pw, with comma", n, "/acs"), true},
		{"digest wrong pass", digestHeader(a, "acs", "nope", n, "/acs"), false},
		{"digest wrong user", digestHeader(a, "x", "pw, with comma", n, "/acs"), false},
		{"digest stale nonce", digestHeader(a, "acs", "pw, with comma", stale, "/acs"), false},
		{"digest forged nonce", digestHeader(a, "acs", "pw, with comma", forged, "/acs"), false},
	} {
		if got := a.Check(req(tc.h)); got != tc.want {
			t.Errorf("%s: Check = %v, want %v", tc.name, got, tc.want)
		}
	}

	w := httptest.NewRecorder()
	a.Challenge(w)
	if h := w.Header().Values("WWW-Authenticate"); len(h) != 2 || !strings.HasPrefix(h[0], "Digest ") {
		t.Errorf("challenge = %v", h)
	}
}

func TestAllowed(t *testing.T) {
	s := &Server{Allow: []netip.Prefix{netip.MustParsePrefix("172.16.106.0/24"), netip.MustParsePrefix("::1/128")}}
	for addr, want := range map[string]bool{
		"172.16.106.10:7547":       true,
		"[::ffff:172.16.106.12]:1": true,
		"10.0.0.1:7547":            false,
		"[::1]:80":                 true,
		"garbage":                  false,
	} {
		if got := s.allowed(addr); got != want {
			t.Errorf("allowed(%q) = %v, want %v", addr, got, want)
		}
	}
	if !(&Server{}).allowed("1.2.3.4:5") {
		t.Error("empty allowlist must accept any source")
	}
}

type stubHandler struct{ informs int }

func (h *stubHandler) Inform(context.Context, *Session, *Inform) error { h.informs++; return nil }
func (h *stubHandler) TransferComplete(context.Context, *Session, *TransferComplete) error {
	return nil
}
func (h *stubHandler) Next(context.Context, *Session) (Request, bool, error) {
	return Request{}, false, nil
}
func (h *stubHandler) Response(context.Context, *Session, Request, *Message) error { return nil }
func (h *stubHandler) Close(*Session, *Request)                                    {}

// The device gets a 401 challenge, answers it on the same connection, and the
// Inform goes through; the handler never sees the unauthenticated attempt.
func TestServerChallengesInform(t *testing.T) {
	h := &stubHandler{}
	s := &Server{Handler: h, MaxBody: 1 << 20, SessionTimeout: time.Second, Auth: NewAuth("acs", "pw")}
	s.Start()
	defer s.Stop()
	srv := httptest.NewUnstartedServer(s)
	srv.Config.ConnContext = s.ConnContext
	srv.Start()
	defer srv.Close()

	inform := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0"><soap-env:Header><cwmp:ID>1</cwmp:ID></soap-env:Header><soap-env:Body><cwmp:Inform><DeviceId><OUI>00259E</OUI><ProductClass>X</ProductClass><SerialNumber>S1</SerialNumber></DeviceId><Event></Event><MaxEnvelopes>1</MaxEnvelopes><CurrentTime>2026-01-01T00:00:00Z</CurrentTime><RetryCount>0</RetryCount><ParameterList></ParameterList></cwmp:Inform></soap-env:Body></soap-env:Envelope>`
	post := func(auth string) *http.Response {
		r, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(inform))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp := post("")
	if resp.StatusCode != http.StatusUnauthorized || h.informs != 0 {
		t.Fatalf("no credentials: status %d, informs %d", resp.StatusCode, h.informs)
	}
	nonce := digestParams(strings.TrimPrefix(resp.Header.Values("WWW-Authenticate")[0], "Digest "))["nonce"]
	if resp := post(digestHeader(s.Auth, "acs", "pw", nonce, "/")); resp.StatusCode != http.StatusOK || h.informs != 1 {
		t.Fatalf("digest: status %d, informs %d", resp.StatusCode, h.informs)
	}
}
