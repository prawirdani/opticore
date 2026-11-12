package cwmp

import (
	"bytes"
	"errors"
	"fmt"
)

// Kind identifies the CWMP RPC carried by a message.
type Kind int

const (
	KindEmpty Kind = iota
	KindInform
	KindGetRPCMethods
	KindTransferComplete
	KindGetParameterValuesResponse
	KindGetParameterNamesResponse
	KindSetParameterValuesResponse
	KindAddObjectResponse
	KindDeleteObjectResponse
	KindRebootResponse
	KindFactoryResetResponse
	KindDownloadResponse
	KindFault
)

func (k Kind) String() string {
	switch k {
	case KindEmpty:
		return "Empty"
	case KindInform:
		return "Inform"
	case KindGetRPCMethods:
		return "GetRPCMethods"
	case KindTransferComplete:
		return "TransferComplete"
	case KindGetParameterValuesResponse:
		return "GetParameterValuesResponse"
	case KindGetParameterNamesResponse:
		return "GetParameterNamesResponse"
	case KindSetParameterValuesResponse:
		return "SetParameterValuesResponse"
	case KindAddObjectResponse:
		return "AddObjectResponse"
	case KindDeleteObjectResponse:
		return "DeleteObjectResponse"
	case KindRebootResponse:
		return "RebootResponse"
	case KindFactoryResetResponse:
		return "FactoryResetResponse"
	case KindDownloadResponse:
		return "DownloadResponse"
	case KindFault:
		return "Fault"
	}
	return "Unknown"
}

// Header is the SOAP header a device may send.
type Header struct {
	ID                string
	HoldRequests      bool
	SessionTimeout    int
	HasSessionTimeout bool
}

type DeviceID struct {
	Manufacturer string
	OUI          string
	ProductClass string
	SerialNumber string
}

type Event struct {
	Code       string
	CommandKey string
}

// ParamValue is a ParameterValueStruct. Type keeps the sent form, e.g.
// "xsd:string"; it defaults to "xsd:string" when the attribute is absent.
type ParamValue struct {
	Name  string
	Value string
	Type  string
}

type ParamInfo struct {
	Name     string
	Writable bool
}

type SetParameterValuesFault struct {
	ParameterName string
	FaultCode     int
	FaultString   string
}

type Inform struct {
	DeviceID     DeviceID
	Events       []Event
	MaxEnvelopes int
	CurrentTime  string
	RetryCount   int
	Params       []ParamValue
}

type GetRPCMethods struct{}

type TransferComplete struct {
	CommandKey   string
	FaultCode    int
	FaultString  string
	StartTime    string
	CompleteTime string
}

type GetParameterValuesResponse struct {
	Params []ParamValue
}

type GetParameterNamesResponse struct {
	Params []ParamInfo
}

type SetParameterValuesResponse struct {
	Status int
}

type AddObjectResponse struct {
	InstanceNumber int
	Status         int
}

type DeleteObjectResponse struct {
	Status int
}

type RebootResponse struct{}

type FactoryResetResponse struct{}

type DownloadResponse struct {
	Status       int
	StartTime    string
	CompleteTime string
}

// Fault is a SOAP fault with the CWMP detail body.
type Fault struct {
	FaultCode               int
	FaultString             string
	SetParameterValuesFault []SetParameterValuesFault
}

// Message is one decoded CWMP message. Kind selects the populated field.
type Message struct {
	Kind    Kind
	Version string // CWMP version, empty when it cannot be resolved
	Header  Header

	Inform                     Inform
	GetRPCMethods              GetRPCMethods
	TransferComplete           TransferComplete
	GetParameterValuesResponse GetParameterValuesResponse
	GetParameterNamesResponse  GetParameterNamesResponse
	SetParameterValuesResponse SetParameterValuesResponse
	AddObjectResponse          AddObjectResponse
	DeleteObjectResponse       DeleteObjectResponse
	RebootResponse             RebootResponse
	FactoryResetResponse       FactoryResetResponse
	DownloadResponse           DownloadResponse
	Fault                      Fault
}

var (
	nsCWMP10    = []byte("urn:dslforum-org:cwmp-1-0")
	nsCWMP11    = []byte("urn:dslforum-org:cwmp-1-1")
	nsCWMP12    = []byte("urn:dslforum-org:cwmp-1-2")
	nsCWMP13    = []byte("urn:dslforum-org:cwmp-1-3")
	cwmpPrefix  = []byte("cwmp")
	errNoEnvel  = errors.New("cwmp: root element is not Envelope")
	errNoBody   = errors.New("cwmp: envelope has no Body")
	errNoMethod = errors.New("cwmp: body has no method element")
)

// Decode parses b into a new Message. All strings are copied out of b.
func Decode(b []byte) (*Message, error) {
	m := &Message{}
	var d Doc
	if err := DecodeInto(b, m, &d); err != nil {
		return nil, err
	}
	return m, nil
}

// DecodeInto parses b using d as scratch. The Doc is fully reusable
// afterwards; m holds copied strings.
func DecodeInto(b []byte, m *Message, d *Doc) error {
	*m = Message{}
	if isAllSpace(b) {
		m.Kind = KindEmpty
		return nil
	}
	if err := d.Parse(b); err != nil {
		return err
	}
	if len(d.Nodes) == 0 {
		return errNoMethod
	}
	root := int32(0)
	if !localIs(&d.Nodes[root], "Envelope") {
		return errNoEnvel
	}
	if h := d.Child(root, "Header"); h >= 0 {
		d.decodeHeader(h, &m.Header)
	}
	body := d.Child(root, "Body")
	if body < 0 {
		return errNoBody
	}
	method := d.Nodes[body].FirstChild
	if method < 0 {
		m.Kind = KindEmpty
		return nil
	}
	kind, ok := d.kindOf(method)
	if !ok {
		return fmt.Errorf("cwmp: unknown method %q", d.Nodes[method].Local)
	}
	m.Kind = kind

	ver, verr := d.detectVersion(method, m.Header.HasSessionTimeout)
	m.Version = ver
	if kind == KindInform && verr != nil {
		return verr
	}

	switch kind {
	case KindInform:
		d.decodeInform(method, &m.Inform)
	case KindGetRPCMethods:
	case KindTransferComplete:
		d.decodeTransferComplete(method, &m.TransferComplete)
	case KindGetParameterValuesResponse:
		m.GetParameterValuesResponse.Params = d.decodeParamValues(method)
	case KindGetParameterNamesResponse:
		d.walk(method, "ParameterInfoStruct", func(i int32) {
			m.GetParameterNamesResponse.Params = append(m.GetParameterNamesResponse.Params, ParamInfo{
				Name:     d.childString(i, "Name"),
				Writable: parseBool(d.childText(i, "Writable")),
			})
		})
	case KindSetParameterValuesResponse:
		m.SetParameterValuesResponse.Status = parseInt(d.childText(method, "Status"))
	case KindAddObjectResponse:
		m.AddObjectResponse.InstanceNumber = parseInt(d.childText(method, "InstanceNumber"))
		m.AddObjectResponse.Status = parseInt(d.childText(method, "Status"))
	case KindDeleteObjectResponse:
		m.DeleteObjectResponse.Status = parseInt(d.childText(method, "Status"))
	case KindRebootResponse:
	case KindFactoryResetResponse:
	case KindDownloadResponse:
		m.DownloadResponse.Status = parseInt(d.childText(method, "Status"))
		m.DownloadResponse.StartTime = d.childString(method, "StartTime")
		m.DownloadResponse.CompleteTime = d.childString(method, "CompleteTime")
	case KindFault:
		d.decodeFault(method, &m.Fault)
	}
	return nil
}

func (d *Doc) kindOf(method int32) (Kind, bool) {
	n := &d.Nodes[method]
	switch {
	case localIs(n, "Inform"):
		return KindInform, true
	case localIs(n, "GetRPCMethods"):
		return KindGetRPCMethods, true
	case localIs(n, "TransferComplete"):
		return KindTransferComplete, true
	case localIs(n, "GetParameterValuesResponse"):
		return KindGetParameterValuesResponse, true
	case localIs(n, "GetParameterNamesResponse"):
		return KindGetParameterNamesResponse, true
	case localIs(n, "SetParameterValuesResponse"):
		return KindSetParameterValuesResponse, true
	case localIs(n, "AddObjectResponse"):
		return KindAddObjectResponse, true
	case localIs(n, "DeleteObjectResponse"):
		return KindDeleteObjectResponse, true
	case localIs(n, "RebootResponse"):
		return KindRebootResponse, true
	case localIs(n, "FactoryResetResponse"):
		return KindFactoryResetResponse, true
	case localIs(n, "DownloadResponse"):
		return KindDownloadResponse, true
	case localIs(n, "Fault"):
		return KindFault, true
	}
	return KindEmpty, false
}

// detectVersion maps the cwmp namespace of the method element to a CWMP
// version. cwmp-1-2 doubled as 1.3; devices signal 1.3 with a sessionTimeout
// header.
func (d *Doc) detectVersion(method int32, hasTimeout bool) (string, error) {
	uri := d.NamespaceURI(method, d.Nodes[method].Prefix)
	if uri == nil {
		uri = d.NamespaceURI(method, cwmpPrefix)
	}
	switch {
	case bytes.Equal(uri, nsCWMP10):
		return "1.0", nil
	case bytes.Equal(uri, nsCWMP11):
		return "1.1", nil
	case bytes.Equal(uri, nsCWMP12):
		if hasTimeout {
			return "1.3", nil
		}
		return "1.2", nil
	case bytes.Equal(uri, nsCWMP13):
		return "1.4", nil
	case uri == nil:
		return "", errors.New("cwmp: cannot resolve the cwmp namespace")
	}
	return "", fmt.Errorf("cwmp: unsupported cwmp namespace %q", uri)
}

func (d *Doc) decodeHeader(i int32, h *Header) {
	h.ID = d.childString(i, "ID")
	if hr := d.Child(i, "HoldRequests"); hr >= 0 {
		h.HoldRequests = parseBool(d.Nodes[hr].Text)
	}
	if st := d.Child(i, "sessionTimeout"); st >= 0 {
		h.SessionTimeout = parseInt(d.Nodes[st].Text)
		h.HasSessionTimeout = true
	}
}

func (d *Doc) decodeInform(method int32, in *Inform) {
	// TR-069 spells it DeviceId; some implementations send DeviceID.
	did := d.Child(method, "DeviceId")
	if did < 0 {
		did = d.Child(method, "DeviceID")
	}
	if did >= 0 {
		in.DeviceID.Manufacturer = d.childString(did, "Manufacturer")
		in.DeviceID.OUI = d.childString(did, "OUI")
		in.DeviceID.ProductClass = d.childString(did, "ProductClass")
		in.DeviceID.SerialNumber = d.childString(did, "SerialNumber")
	}
	d.walk(method, "EventStruct", func(i int32) {
		in.Events = append(in.Events, Event{
			Code:       d.childString(i, "EventCode"),
			CommandKey: d.childString(i, "CommandKey"),
		})
	})
	in.MaxEnvelopes = parseInt(d.childText(method, "MaxEnvelopes"))
	in.CurrentTime = d.childString(method, "CurrentTime")
	in.RetryCount = parseInt(d.childText(method, "RetryCount"))
	in.Params = d.decodeParamValues(method)
}

func (d *Doc) decodeParamValues(method int32) []ParamValue {
	var out []ParamValue
	d.walk(method, "ParameterValueStruct", func(i int32) {
		p := ParamValue{Name: d.childString(i, "Name")}
		if v := d.Child(i, "Value"); v >= 0 {
			p.Value = string(d.Nodes[v].Text)
			if t, ok := d.Attr(v, "type"); ok {
				p.Type = string(t)
			}
		}
		if p.Type == "" {
			p.Type = "xsd:string"
		}
		out = append(out, p)
	})
	return out
}

func (d *Doc) decodeTransferComplete(method int32, tc *TransferComplete) {
	tc.CommandKey = d.childString(method, "CommandKey")
	if fs := d.Child(method, "FaultStruct"); fs >= 0 {
		tc.FaultCode = parseInt(d.childText(fs, "FaultCode"))
		tc.FaultString = d.childString(fs, "FaultString")
	}
	tc.StartTime = d.childString(method, "StartTime")
	tc.CompleteTime = d.childString(method, "CompleteTime")
}

func (d *Doc) decodeFault(method int32, f *Fault) {
	f.FaultString = d.childString(method, "faultstring")
	det := d.Child(method, "detail")
	if det < 0 {
		return
	}
	cf := d.Child(det, "Fault")
	if cf < 0 {
		cf = d.Descendant(det, "Fault")
	}
	if cf < 0 {
		return
	}
	f.FaultCode = parseInt(d.childText(cf, "FaultCode"))
	if s := d.childString(cf, "FaultString"); s != "" {
		f.FaultString = s
	}
	d.walk(cf, "SetParameterValuesFault", func(i int32) {
		f.SetParameterValuesFault = append(f.SetParameterValuesFault, SetParameterValuesFault{
			ParameterName: d.childString(i, "ParameterName"),
			FaultCode:     parseInt(d.childText(i, "FaultCode")),
			FaultString:   d.childString(i, "FaultString"),
		})
	})
}

// walk calls fn for every descendant of i with that local name, in document
// order. Matched nodes are not searched further.
func (d *Doc) walk(i int32, local string, fn func(int32)) {
	for c := d.Nodes[i].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
		if localIs(&d.Nodes[c], local) {
			fn(c)
			continue
		}
		d.walk(c, local, fn)
	}
}

func (d *Doc) childText(i int32, local string) []byte {
	if c := d.Child(i, local); c >= 0 {
		return d.Nodes[c].Text
	}
	return nil
}

func (d *Doc) childString(i int32, local string) string {
	return string(d.childText(i, local))
}

func parseInt(b []byte) int {
	b = trimSpace(b)
	if len(b) == 0 {
		return 0
	}
	i := 0
	neg := false
	if b[0] == '-' {
		neg = true
		i++
	} else if b[0] == '+' {
		i++
	}
	n := 0
	for ; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			return 0
		}
		n = n*10 + int(b[i]-'0')
		if n > maxInt32 {
			return 0
		}
	}
	if neg {
		return -n
	}
	return n
}

func parseBool(b []byte) bool {
	b = trimSpace(b)
	return len(b) == 1 && b[0] == '1' || string(b) == "true"
}
