package cwmp

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const serverName = "Opticore"

// Server is the device-facing CWMP endpoint (an http.Handler). It owns
// transport, session tracking and RPC sequencing; Handler owns the domain.
type Server struct {
	Handler        Handler
	Logger         *slog.Logger
	MaxBody        int64         // decompressed body limit
	SessionTimeout time.Duration // idle limit between POSTs
	MaxDuration    time.Duration // hard session cap
	MaxSessions    int

	mu       sync.Mutex
	byID     map[string]*session
	byDevice map[string]*session
	stop     chan struct{}
}

type session struct {
	Session
	mu           sync.Mutex // serialises POSTs of one session
	lastActivity time.Time
	inflight     *Request
	seq          uint64
	ended        bool
}

type connKey struct{}

// connRef ties a TCP connection to the session it carries.
type connRef struct {
	mu   sync.Mutex
	sess *session
}

// ConnContext must be set as http.Server.ConnContext so sessions are tracked
// per TCP connection first, cookie second (as GenieACS does).
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, &connRef{})
}

// Start launches the idle-session sweeper; Stop ends it and closes sessions.
func (s *Server) Start() {
	s.mu.Lock()
	s.byID = map[string]*session{}
	s.byDevice = map[string]*session{}
	s.stop = make(chan struct{})
	s.mu.Unlock()
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.MaxDuration == 0 {
		s.MaxDuration = 300 * time.Second
	}
	go s.sweep()
}

func (s *Server) Stop() {
	close(s.stop)
	s.mu.Lock()
	all := make([]*session, 0, len(s.byID))
	for _, ss := range s.byID {
		all = append(all, ss)
	}
	s.mu.Unlock()
	for _, ss := range all {
		ss.mu.Lock()
		s.end(ss)
		ss.mu.Unlock()
	}
}

func (s *Server) sweep() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.mu.Lock()
			var stale []*session
			for _, ss := range s.byID {
				stale = append(stale, ss)
			}
			s.mu.Unlock()
			for _, ss := range stale {
				if !ss.mu.TryLock() { // a POST is being handled: not idle
					continue
				}
				if now.Sub(ss.lastActivity) > s.SessionTimeout || now.Sub(ss.Started) > s.MaxDuration {
					s.Logger.Info("cwmp session timeout", "device_id", ss.DeviceID, "session", ss.ID)
					s.end(ss)
				}
				ss.mu.Unlock()
			}
		}
	}
}

// end removes ss and calls Handler.Close once. Caller holds ss.mu.
func (s *Server) end(ss *session) {
	if ss.ended {
		return
	}
	ss.ended = true
	s.mu.Lock()
	delete(s.byID, ss.ID)
	if s.byDevice[ss.DeviceID] == ss {
		delete(s.byDevice, ss.DeviceID)
	}
	s.mu.Unlock()
	s.Handler.Close(&ss.Session, ss.inflight)
	ss.inflight = nil
}

var (
	docPool = sync.Pool{New: func() any { return new(Doc) }}
	msgPool = sync.Pool{New: func() any { return new(Message) }}
	bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 8192); return &b }}
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	body, status, err := s.readBody(r)
	if err != nil {
		s.fail(w, r, status, err.Error())
		return
	}

	ref, _ := r.Context().Value(connKey{}).(*connRef)
	ss := s.lookup(r, ref)

	d := docPool.Get().(*Doc)
	m := msgPool.Get().(*Message)
	defer docPool.Put(d)
	defer msgPool.Put(m)
	if err := DecodeInto(body, m, d); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}

	if m.Kind == KindInform {
		if ss != nil { // a new Inform ends whatever this connection carried
			ss.mu.Lock()
			s.end(ss)
			ss.mu.Unlock()
		}
		s.inform(w, r, ref, m)
		return
	}
	if ss == nil {
		s.fail(w, r, http.StatusBadRequest, "no session: first message must be Inform")
		return
	}

	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.ended {
		s.fail(w, r, http.StatusBadRequest, "session ended")
		return
	}
	ss.lastActivity = time.Now()
	ctx := r.Context()

	switch m.Kind {
	case KindGetRPCMethods:
		s.write(w, r, AppendGetRPCMethodsResponse(s.buf(), ss.Version, m.Header.ID), false)
		return
	case KindTransferComplete:
		if err := s.Handler.TransferComplete(ctx, &ss.Session, &m.TransferComplete); err != nil {
			s.Logger.Error("transfer complete", "device_id", ss.DeviceID, "err", err)
			s.end(ss)
			s.fail(w, r, http.StatusServiceUnavailable, "storage unavailable")
			return
		}
		s.write(w, r, AppendTransferCompleteResponse(s.buf(), ss.Version, m.Header.ID), false)
		return
	case KindEmpty:
		// CPE has nothing more to send; fall through to our requests.
	default: // a response or fault to our request
		if ss.inflight == nil {
			s.Logger.Warn("unsolicited cpe message", "device_id", ss.DeviceID, "kind", m.Kind.String())
			break
		}
		req := *ss.inflight
		ss.inflight = nil
		if err := s.Handler.Response(ctx, &ss.Session, req, m); err != nil {
			s.Logger.Error("handle response", "device_id", ss.DeviceID, "err", err)
			s.end(ss)
			s.fail(w, r, http.StatusServiceUnavailable, "storage unavailable")
			return
		}
	}

	req, ok, err := s.Handler.Next(ctx, &ss.Session)
	if err != nil {
		s.Logger.Error("next request", "device_id", ss.DeviceID, "err", err)
	}
	if err != nil || !ok {
		s.end(ss)
		s.clearConn(ref, ss)
		s.write(w, r, nil, true)
		return
	}
	ss.seq++
	id := ss.ID[:8] + "-" + strconv.FormatUint(ss.seq, 10)
	out, err := appendRequest(s.buf(), ss.Version, id, &req)
	if err != nil {
		s.Logger.Error("encode request", "device_id", ss.DeviceID, "err", err)
		s.end(ss)
		s.write(w, r, nil, true)
		return
	}
	ss.inflight = &req
	s.write(w, r, out, false)
}

func (s *Server) inform(w http.ResponseWriter, r *http.Request, ref *connRef, m *Message) {
	in := &m.Inform
	if m.Version == "" {
		s.fail(w, r, http.StatusBadRequest, "unrecognised CWMP version")
		return
	}
	if in.DeviceID.SerialNumber == "" || in.DeviceID.OUI == "" {
		s.fail(w, r, http.StatusBadRequest, "Inform without DeviceId")
		return
	}
	ss := &session{
		Session: Session{
			ID:         newID(),
			DeviceID:   DeviceIDOf(in.DeviceID),
			Version:    m.Version,
			RemoteAddr: hostOnly(r.RemoteAddr),
			Started:    time.Now(),
		},
		lastActivity: time.Now(),
	}

	s.mu.Lock()
	if old := s.byDevice[ss.DeviceID]; old != nil {
		s.mu.Unlock()
		// An idle old session means the device gave up on it and restarted
		// (e.g. our InformResponse was lost): replace it. A busy one is a
		// genuine concurrent session: refuse.
		if !old.mu.TryLock() {
			s.fail(w, r, http.StatusBadRequest, "a session for this device is already in progress")
			return
		}
		s.end(old)
		old.mu.Unlock()
		s.mu.Lock()
	}
	if s.MaxSessions > 0 && len(s.byID) >= s.MaxSessions {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		s.fail(w, r, http.StatusServiceUnavailable, "too many sessions")
		return
	}
	s.byID[ss.ID] = ss
	s.byDevice[ss.DeviceID] = ss
	s.mu.Unlock()

	ss.mu.Lock()
	defer ss.mu.Unlock()
	if err := s.Handler.Inform(r.Context(), &ss.Session, in); err != nil {
		s.Logger.Error("record inform", "device_id", ss.DeviceID, "err", err)
		s.end(ss)
		w.Header().Set("Retry-After", "60")
		s.fail(w, r, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	if ref != nil {
		ref.mu.Lock()
		ref.sess = ss
		ref.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: ss.ID, Path: "/"})
	s.write(w, r, AppendInformResponse(s.buf(), ss.Version, m.Header.ID), false)
}

func (s *Server) lookup(r *http.Request, ref *connRef) *session {
	if ref != nil {
		ref.mu.Lock()
		ss := ref.sess
		ref.mu.Unlock()
		if ss != nil {
			return ss
		}
	}
	c, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	s.mu.Lock()
	ss := s.byID[c.Value]
	s.mu.Unlock()
	if ss != nil && ref != nil {
		ref.mu.Lock()
		ref.sess = ss
		ref.mu.Unlock()
	}
	return ss
}

func (s *Server) clearConn(ref *connRef, ss *session) {
	if ref == nil {
		return
	}
	ref.mu.Lock()
	if ref.sess == ss {
		ref.sess = nil
	}
	ref.mu.Unlock()
}

func (s *Server) buf() []byte {
	return (*bufPool.Get().(*[]byte))[:0]
}

// write sends an envelope (or 204 when body is nil), compressed like the
// request, always with Content-Length.
func (s *Server) write(w http.ResponseWriter, r *http.Request, body []byte, last bool) {
	h := w.Header()
	h.Set("Server", serverName)
	h.Set("SOAPServer", serverName)
	if last {
		h.Set("Connection", "close")
	}
	if body == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer func() { bufPool.Put(&body) }()
	out := body
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "gzip":
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		out = b.Bytes()
		h.Set("Content-Encoding", "gzip")
	case "deflate":
		var b bytes.Buffer
		zw, _ := flate.NewWriter(&b, flate.DefaultCompression)
		zw.Write(body)
		zw.Close()
		out = b.Bytes()
		h.Set("Content-Encoding", "deflate")
	}
	h.Set("Content-Type", `text/xml; charset="utf-8"`)
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.Logger.Warn("cwmp request rejected", "status", code, "remote", r.RemoteAddr, "reason", msg)
	w.Header().Set("Connection", "close")
	http.Error(w, msg, code)
}

var errTooLarge = errors.New("request body too large")

// readBody decompresses, size-limits and converts the body to UTF-8.
func (s *Server) readBody(r *http.Request) ([]byte, int, error) {
	var src io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		defer zr.Close()
		src = zr
	case "deflate":
		zr := flate.NewReader(r.Body)
		defer zr.Close()
		src = zr
	default:
		return nil, http.StatusUnsupportedMediaType, errors.New("unsupported Content-Encoding " + enc)
	}
	limit := s.MaxBody
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if int64(len(body)) > limit {
		return nil, http.StatusRequestEntityTooLarge, errTooLarge
	}
	body, err = toUTF8(body, r.Header.Get("Content-Type"))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return body, 0, nil
}

// toUTF8 picks the charset from Content-Type, then the XML declaration, then
// a UTF-16 byte-order mark, and defaults to UTF-8.
func toUTF8(b []byte, contentType string) ([]byte, error) {
	charset := ""
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		charset = params["charset"]
	}
	if charset == "" {
		switch {
		case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
			charset = "utf-16le"
		case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
			charset = "utf-16be"
		case bytes.HasPrefix(b, []byte("<?xml")):
			charset = xmlDeclEncoding(b)
		}
	}
	switch strings.ToLower(strings.Trim(charset, `"' `)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}), nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252":
		if !hasHighBit(b) {
			return b, nil
		}
		out := make([]byte, 0, len(b)+len(b)/8)
		for _, c := range b {
			out = utf8.AppendRune(out, rune(c))
		}
		return out, nil
	case "utf-16", "utf-16le":
		return fromUTF16(b, false)
	case "utf-16be":
		return fromUTF16(b, true)
	default:
		return nil, errors.New("unknown encoding " + charset)
	}
}

func xmlDeclEncoding(b []byte) string {
	end := bytes.Index(b, []byte("?>"))
	if end < 0 {
		return ""
	}
	decl := b[:end]
	i := bytes.Index(decl, []byte("encoding"))
	if i < 0 {
		return ""
	}
	rest := bytes.TrimLeft(decl[i+len("encoding"):], " \t=")
	if len(rest) == 0 || (rest[0] != '"' && rest[0] != '\'') {
		return ""
	}
	q := rest[0]
	j := bytes.IndexByte(rest[1:], q)
	if j < 0 {
		return ""
	}
	return string(rest[1 : 1+j])
}

func hasHighBit(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

func fromUTF16(b []byte, bigEndian bool) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("odd-length UTF-16 body")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	if len(u) > 0 && u[0] == 0xFEFF {
		u = u[1:]
	}
	return []byte(string(utf16.Decode(u))), nil
}

// appendRequest encodes an ACS request envelope.
func appendRequest(dst []byte, ver, id string, req *Request) ([]byte, error) {
	switch req.Kind {
	case ReqGetParameterValues:
		return AppendGetParameterValues(dst, ver, id, req.Names), nil
	case ReqGetParameterNames:
		return AppendGetParameterNames(dst, ver, id, req.Path, req.NextLevel), nil
	case ReqSetParameterValues:
		return AppendSetParameterValues(dst, ver, id, req.Values, req.Key), nil
	case ReqAddObject:
		return AppendAddObject(dst, ver, id, req.Path, req.Key), nil
	case ReqDeleteObject:
		return AppendDeleteObject(dst, ver, id, req.Path, req.Key), nil
	case ReqReboot:
		return AppendReboot(dst, ver, id, req.Key), nil
	case ReqFactoryReset:
		return AppendFactoryReset(dst, ver, id), nil
	case ReqDownload:
		a := req.Download
		return AppendDownload(dst, ver, id, req.Key, a.FileType, a.URL, a.Username, a.Password,
			a.FileSize, a.TargetFileName, a.DelaySeconds, a.SuccessURL, a.FailureURL), nil
	}
	return nil, errors.New("cwmp: unknown request kind " + strconv.Itoa(int(req.Kind)))
}

// DeviceIDOf builds the GenieACS-compatible ACS id: OUI-ProductClass-Serial,
// each part with everything except [A-Za-z0-9_] percent-encoded.
func DeviceIDOf(d DeviceID) string {
	if d.ProductClass == "" {
		return escID(d.OUI) + "-" + escID(d.SerialNumber)
	}
	return escID(d.OUI) + "-" + escID(d.ProductClass) + "-" + escID(d.SerialNumber)
}

func escID(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteByte(c)
			continue
		}
		// GenieACS uses byte.toString(16): no zero padding, so 0x0A is "%A".
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(strconv.FormatUint(uint64(c), 16)))
	}
	return b.String()
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
