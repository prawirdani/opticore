package cwmp

import (
	"context"
	"time"
)

// RequestKind is an ACS-to-CPE RPC.
type RequestKind int

const (
	ReqGetParameterValues RequestKind = iota + 1
	ReqGetParameterNames
	ReqSetParameterValues
	ReqAddObject
	ReqDeleteObject
	ReqReboot
	ReqFactoryReset
	ReqDownload
)

// Request is one RPC the handler wants sent to the device. Only the fields of
// its Kind are used. Tag is opaque to the server and handed back in Response.
type Request struct {
	Kind RequestKind

	Names     []string     // GetParameterValues
	Path      string       // GetParameterNames path; AddObject/DeleteObject object name
	NextLevel bool         // GetParameterNames
	Values    []ParamValue // SetParameterValues
	Key       string       // ParameterKey (SPV/AddObject/DeleteObject) or CommandKey (Reboot/Download)

	Download DownloadArgs

	Tag any
}

// DownloadArgs are the arguments of a Download RPC.
type DownloadArgs struct {
	FileType       string
	URL            string
	Username       string
	Password       string
	FileSize       int64
	TargetFileName string
	DelaySeconds   int
	SuccessURL     string
	FailureURL     string
}

// Session is one CWMP session. The server owns ID, DeviceID, Version,
// RemoteAddr and Started; the handler may keep its own state in Data.
type Session struct {
	ID         string
	DeviceID   string // GenieACS-compatible ACS id, see DeviceIDOf
	Version    string // CWMP version from the Inform
	RemoteAddr string // device IP, no port
	Started    time.Time
	Data       any
}

// Handler is the domain side of a CWMP session. The server guarantees calls
// for one session are sequential, and that at most one Request is in flight.
type Handler interface {
	// Inform records the Inform. An error answers 503 and ends the session.
	// in and m (below) are pooled: do not retain them after returning.
	Inform(ctx context.Context, s *Session, in *Inform) error
	// TransferComplete records a CPE TransferComplete.
	TransferComplete(ctx context.Context, s *Session, tc *TransferComplete) error
	// Next returns the next RPC to send, or ok=false to end the session.
	Next(ctx context.Context, s *Session) (req Request, ok bool, err error)
	// Response delivers the CPE's answer to req. m.Kind is the matching
	// response kind or KindFault.
	Response(ctx context.Context, s *Session, req Request, m *Message) error
	// Close is called exactly once per session. inflight is the Request sent
	// but never answered (timeout, dropped connection), or nil.
	Close(s *Session, inflight *Request)
}
