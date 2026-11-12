package wake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUser  = "acs-user"
	testPass  = "s3cr3t-pass"
	testRealm = "acs"
	testNonce = "nonce-1"
)

// verifyDigest recomputes the RFC 2617 response so tests check real hashes.
func verifyDigest(r *http.Request, user, pass, realm, nonce, qop string) (bool, map[string]string) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false, nil
	}
	params := map[string]string{}
	for _, item := range authItems(h[len("Digest "):]) {
		if k, v, ok := cutParam(item); ok {
			params[k] = v
		}
	}
	uri := r.URL.RequestURI()
	if params["username"] != user || params["realm"] != realm ||
		params["nonce"] != nonce || params["uri"] != uri {
		return false, params
	}
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex(r.Method + ":" + uri)
	var want string
	if qop != "" {
		want = md5hex(ha1 + ":" + nonce + ":" + params["nc"] + ":" + params["cnonce"] + ":" + qop + ":" + ha2)
	} else {
		want = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return params["response"] == want, params
}

func challenge(w http.ResponseWriter, nonce string, stale bool) {
	v := fmt.Sprintf(`Digest realm=%q, nonce=%q, qop="auth", algorithm=MD5`, testRealm, nonce)
	if stale {
		v += ", stale=true"
	}
	w.Header().Set("WWW-Authenticate", v)
	w.WriteHeader(http.StatusUnauthorized)
}

// digestServer challenges once and accepts a correctly signed request.
func digestServer(t *testing.T, reqs *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(reqs, 1)
		if ok, _ := verifyDigest(r, testUser, testPass, testRealm, testNonce, "auth"); ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		challenge(w, testNonce, false)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func target(url string) Target {
	return Target{DeviceID: "dev-1", URL: url, Username: testUser, Password: testPass}
}

func TestUnauthenticatedOK(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization on open endpoint")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := New(time.Second, nil)
	defer w.Close()
	if err := w.Wake(context.Background(), target(srv.URL)); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestDigestChallengeThenSuccess(t *testing.T) {
	var reqs int32
	srv := digestServer(t, &reqs)
	w := New(time.Second, nil)
	defer w.Close()

	if err := w.Wake(context.Background(), target(srv.URL+"/cr?x=1")); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestCachedNoncePreSent(t *testing.T) {
	var reqs int32
	var mu sync.Mutex
	var nonceCounts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		ok, params := verifyDigest(r, testUser, testPass, testRealm, testNonce, "auth")
		if ok {
			mu.Lock()
			nonceCounts = append(nonceCounts, params["nc"])
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		challenge(w, testNonce, false)
	}))
	defer srv.Close()

	w := New(time.Second, nil)
	defer w.Close()
	tgt := target(srv.URL + "/cr")

	if err := w.Wake(context.Background(), tgt); err != nil {
		t.Fatalf("first Wake: %v", err)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("first Wake requests = %d, want 2", got)
	}

	if err := w.Wake(context.Background(), tgt); err != nil {
		t.Fatalf("second Wake: %v", err)
	}
	if got := atomic.LoadInt32(&reqs); got != 3 {
		t.Fatalf("second Wake requests = %d, want 3 (pre-sent auth)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"00000001", "00000002"}
	if len(nonceCounts) != len(want) {
		t.Fatalf("nc values = %v, want %v", nonceCounts, want)
	}
	for i := range want {
		if nonceCounts[i] != want[i] {
			t.Fatalf("nc values = %v, want %v", nonceCounts, want)
		}
	}
}

func TestStaleRechallenge(t *testing.T) {
	var reqs int32
	var mu sync.Mutex
	nonce := testNonce

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		mu.Lock()
		current := nonce
		mu.Unlock()
		if ok, _ := verifyDigest(r, testUser, testPass, testRealm, current, "auth"); ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		challenge(w, current, true)
	}))
	defer srv.Close()

	w := New(time.Second, nil)
	defer w.Close()
	tgt := target(srv.URL)

	if err := w.Wake(context.Background(), tgt); err != nil {
		t.Fatalf("first Wake: %v", err)
	}
	mu.Lock()
	nonce = "nonce-2"
	mu.Unlock()
	before := atomic.LoadInt32(&reqs)

	if err := w.Wake(context.Background(), tgt); err != nil {
		t.Fatalf("stale Wake: %v", err)
	}
	// Pre-sent stale nonce, then fresh nonce.
	if got := atomic.LoadInt32(&reqs) - before; got != 2 {
		t.Fatalf("stale Wake requests = %d, want 2", got)
	}
}

func TestWrongPasswordUnauthorized(t *testing.T) {
	var reqs int32
	srv := digestServer(t, &reqs)
	w := New(time.Second, nil)
	defer w.Close()

	tgt := target(srv.URL)
	tgt.Password = "wrong"
	err := w.Wake(context.Background(), tgt)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestBasicRejectedUnlessAllowed(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		if u, p, ok := r.BasicAuth(); ok && u == testUser && p == testPass {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Run("rejected", func(t *testing.T) {
		w := New(time.Second, nil)
		defer w.Close()
		atomic.StoreInt32(&reqs, 0)
		err := w.Wake(context.Background(), target(srv.URL))
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
		if got := atomic.LoadInt32(&reqs); got != 1 {
			t.Fatalf("requests = %d, want 1 (no Basic attempt)", got)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		w := New(time.Second, nil)
		defer w.Close()
		atomic.StoreInt32(&reqs, 0)
		tgt := target(srv.URL)
		tgt.AllowBasic = true
		if err := w.Wake(context.Background(), tgt); err != nil {
			t.Fatalf("Wake: %v", err)
		}
		if got := atomic.LoadInt32(&reqs); got != 2 {
			t.Fatalf("requests = %d, want 2", got)
		}
	})
}

func TestRedirectNotFollowed(t *testing.T) {
	var targetHits int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
	}))
	defer targetSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL, http.StatusFound)
	}))
	defer srv.Close()

	w := New(time.Second, nil)
	defer w.Close()
	err := w.Wake(context.Background(), target(srv.URL))

	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Fatalf("err = %v, want StatusError 302", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestTimeoutRetriesOnce(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := New(20*time.Millisecond, nil)
	defer w.Close()
	err := w.Wake(context.Background(), target(srv.URL))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestCoalescing(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := New(time.Second, nil)
	defer w.Close()
	w.Window = 100 * time.Millisecond

	tgt := target(srv.URL)
	for i := 0; i < 5; i++ {
		w.Request(tgt)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&reqs) >= 1 })
	time.Sleep(250 * time.Millisecond)
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestPasswordNotLogged(t *testing.T) {
	var reqs int32
	srv := digestServer(t, &reqs)

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := New(time.Second, logger)
	defer w.Close()
	w.Window = 20 * time.Millisecond

	w.Request(target(srv.URL))
	waitFor(t, func() bool {
		return atomic.LoadInt32(&reqs) == 2 && strings.Contains(buf.String(), "wake")
	})

	out := buf.String()
	if out == "" {
		t.Fatal("expected at least one log line")
	}
	if strings.Contains(out, testPass) {
		t.Fatalf("password leaked into logs: %s", out)
	}
}

// syncBuffer makes the logger's writes safe to read from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
