package cwmp

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeFixture(t *testing.T, name string) *Message {
	t.Helper()
	m, err := Decode(fixture(t, name))
	if err != nil {
		t.Fatalf("Decode(%s): %v", name, err)
	}
	return m
}

func paramByName(t *testing.T, params []ParamValue, name string) ParamValue {
	t.Helper()
	for _, p := range params {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("param %q not found", name)
	return ParamValue{}
}

func TestDecodeInformBoot(t *testing.T) {
	m := decodeFixture(t, "inform_boot.xml")
	if m.Kind != KindInform || m.Version != "1.0" {
		t.Fatalf("kind/version = %v/%q", m.Kind, m.Version)
	}
	if m.Header.ID != "20180411093012" {
		t.Fatalf("id = %q", m.Header.ID)
	}
	if m.Header.HoldRequests || m.Header.HasSessionTimeout {
		t.Fatalf("unexpected header %+v", m.Header)
	}
	in := &m.Inform
	if in.DeviceID.Manufacturer != "Huawei Technologies Co., Ltd." ||
		in.DeviceID.OUI != "00259E" ||
		in.DeviceID.ProductClass != "HG8245H" ||
		in.DeviceID.SerialNumber != "48575443A1B2C3D4" {
		t.Fatalf("device id = %+v", in.DeviceID)
	}
	if len(in.Events) != 2 || in.Events[0].Code != "0 BOOTSTRAP" || in.Events[1].Code != "1 BOOT" {
		t.Fatalf("events = %+v", in.Events)
	}
	if in.MaxEnvelopes != 1 || in.CurrentTime != "2024-03-11T09:12:44+00:00" || in.RetryCount != 0 {
		t.Fatalf("inform scalars = %+v", in)
	}
	if len(in.Params) != 25 {
		t.Fatalf("params = %d", len(in.Params))
	}
	cr := paramByName(t, in.Params, "Device.ManagementServer.ConnectionRequestURL")
	if cr.Value != "http://10.12.34.56:7547/" || cr.Type != "xsd:string" {
		t.Fatalf("ConnectionRequestURL = %+v", cr)
	}
	kp := paramByName(t, in.Params, "Device.WiFi.AccessPoint.1.Security.KeyPassphrase")
	if kp.Value != "secret & more" {
		t.Fatalf("passphrase = %q", kp.Value)
	}
	if got := paramByName(t, in.Params, "Device.DeviceInfo.UpTime"); got.Type != "xsd:unsignedInt" || got.Value != "4212" {
		t.Fatalf("UpTime = %+v", got)
	}
}

func TestDecodeInformCwmp12Is13(t *testing.T) {
	m := decodeFixture(t, "inform_periodic_cwmp12.xml")
	if m.Version != "1.3" {
		t.Fatalf("version = %q, want 1.3", m.Version)
	}
	if !m.Header.HasSessionTimeout || m.Header.SessionTimeout != 120 {
		t.Fatalf("header = %+v", m.Header)
	}
	if m.Header.HoldRequests {
		t.Fatal("HoldRequests should be false")
	}
	if m.Inform.RetryCount != 1 || len(m.Inform.Params) != 2 {
		t.Fatalf("inform = %+v", m.Inform)
	}
}

func TestDecodeOddPrefixes(t *testing.T) {
	m := decodeFixture(t, "odd_prefixes.xml")
	if m.Kind != KindInform || m.Version != "1.1" {
		t.Fatalf("kind/version = %v/%q", m.Kind, m.Version)
	}
	if m.Header.ID != "4471" {
		t.Fatalf("id = %q", m.Header.ID)
	}
	if len(m.Inform.Params) != 2 {
		t.Fatalf("params = %d", len(m.Inform.Params))
	}
	if got := m.Inform.Params[0]; got.Name != "Device.DeviceInfo.UpTime" || got.Type != "XSD:unsignedInt" || got.Value != "17" {
		t.Fatalf("param = %+v", got)
	}
}

func TestDecodeGPVArray(t *testing.T) {
	m := decodeFixture(t, "gpv_response_array.xml")
	if m.Kind != KindGetParameterValuesResponse {
		t.Fatalf("kind = %v", m.Kind)
	}
	p := m.GetParameterValuesResponse.Params
	if len(p) != 200 {
		t.Fatalf("params = %d", len(p))
	}
	if got := p[10]; got.Name != "Device.WiFi.AccessPoint.1.Security.KeyPassphrase" || got.Value != "p&ss<word>" {
		t.Fatalf("p[10] = %+v", got)
	}
	if got := p[50].Value; got != "WAN & IP" {
		t.Fatalf("p[50] = %q", got)
	}
	if got := p[120].Value; got != "Rate: $5 < 10 A" {
		t.Fatalf("p[120] = %q", got)
	}
	if got := p[150]; got.Name != "Device.WiFi.SSID.1.SSID" || got.Value != "CDATA & value" {
		t.Fatalf("p[150] = %+v", got)
	}
	if got := p[0].Type; got != "xsd:int" {
		t.Fatalf("type = %q", got)
	}
}

func TestDecodeGPVPlain(t *testing.T) {
	m := decodeFixture(t, "gpv_response_plain.xml")
	p := m.GetParameterValuesResponse.Params
	if len(p) != 4 {
		t.Fatalf("params = %d", len(p))
	}
	if got := p[1]; got.Value != "secret & more" || got.Type != "xsd:string" {
		t.Fatalf("param = %+v", got)
	}
	if got := p[2]; got.Type != "xsd:boolean" || got.Value != "true" {
		t.Fatalf("param = %+v", got)
	}
}

func TestDecodeGPN(t *testing.T) {
	m := decodeFixture(t, "gpn_response.xml")
	if m.Kind != KindGetParameterNamesResponse || m.Version != "1.1" {
		t.Fatalf("kind/version = %v/%q", m.Kind, m.Version)
	}
	p := m.GetParameterNamesResponse.Params
	if len(p) != 5 {
		t.Fatalf("params = %d", len(p))
	}
	if p[2].Name != "Device.WiFi.SSID.1.SSID" || !p[2].Writable {
		t.Fatalf("p[2] = %+v", p[2])
	}
	if p[0].Writable {
		t.Fatalf("p[0] = %+v", p[0])
	}
}

func TestDecodeSPV(t *testing.T) {
	m := decodeFixture(t, "spv_response.xml")
	if m.Kind != KindSetParameterValuesResponse || m.SetParameterValuesResponse.Status != 1 {
		t.Fatalf("message = %+v", m)
	}
}

func TestDecodeAddObject(t *testing.T) {
	m := decodeFixture(t, "addobject_response.xml")
	if m.Kind != KindAddObjectResponse {
		t.Fatalf("kind = %v", m.Kind)
	}
	if m.AddObjectResponse.InstanceNumber != 7 || m.AddObjectResponse.Status != 0 {
		t.Fatalf("response = %+v", m.AddObjectResponse)
	}
}

func TestDecodeFault(t *testing.T) {
	m := decodeFixture(t, "fault_spv.xml")
	if m.Kind != KindFault {
		t.Fatalf("kind = %v", m.Kind)
	}
	f := m.Fault
	if f.FaultCode != 9003 || f.FaultString != "Invalid arguments" {
		t.Fatalf("fault = %+v", f)
	}
	if len(f.SetParameterValuesFault) != 2 {
		t.Fatalf("spv faults = %d", len(f.SetParameterValuesFault))
	}
	if got := f.SetParameterValuesFault[0]; got.FaultCode != 9007 || got.ParameterName != "Device.ManagementServer.PeriodicInformInterval" || got.FaultString != "Invalid value" {
		t.Fatalf("spv[0] = %+v", got)
	}
	if got := f.SetParameterValuesFault[1]; got.FaultCode != 9006 {
		t.Fatalf("spv[1] = %+v", got)
	}
}

func TestDecodeTransferComplete(t *testing.T) {
	m := decodeFixture(t, "transfer_complete.xml")
	if m.Kind != KindTransferComplete || m.Version != "1.2" {
		t.Fatalf("kind/version = %v/%q", m.Kind, m.Version)
	}
	tc := m.TransferComplete
	if tc.CommandKey != "firmware-2024-03" || tc.FaultCode != 0 || tc.FaultString != "" {
		t.Fatalf("tc = %+v", tc)
	}
	if tc.StartTime != "2024-03-11T09:14:00+00:00" || tc.CompleteTime != "2024-03-11T09:19:32+00:00" {
		t.Fatalf("tc times = %+v", tc)
	}
}

func TestDecodeGetRPCMethods(t *testing.T) {
	m := decodeFixture(t, "getrpcmethods.xml")
	if m.Kind != KindGetRPCMethods || m.Version != "1.0" {
		t.Fatalf("kind/version = %v/%q", m.Kind, m.Version)
	}
}

func TestDecodeEmptyBody(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		m, err := Decode([]byte(in))
		if err != nil {
			t.Fatalf("Decode(%q): %v", in, err)
		}
		if m.Kind != KindEmpty {
			t.Fatalf("kind = %v for %q", m.Kind, in)
		}
	}
	env := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/"><soap-env:Body></soap-env:Body></soap-env:Envelope>`
	m, err := Decode([]byte(env))
	if err != nil || m.Kind != KindEmpty {
		t.Fatalf("empty Body: %v %+v", err, m)
	}
}

func TestDecodeUnknownMethod(t *testing.T) {
	in := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0"><soap-env:Body><cwmp:ScheduleInform/></soap-env:Body></soap-env:Envelope>`
	_, err := Decode([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "ScheduleInform") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeUnsupportedNamespace(t *testing.T) {
	in := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-9-9"><soap-env:Body><cwmp:Inform><cwmp:DeviceID/></cwmp:Inform></soap-env:Body></soap-env:Envelope>`
	if _, err := Decode([]byte(in)); err == nil {
		t.Fatal("want namespace error")
	}
}

func TestDecodeNonInformToleratesBadNamespace(t *testing.T) {
	in := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:example"><soap-env:Body><cwmp:SetParameterValuesResponse><cwmp:Status>0</cwmp:Status></cwmp:SetParameterValuesResponse></soap-env:Body></soap-env:Envelope>`
	m, err := Decode([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "" || m.SetParameterValuesResponse.Status != 0 {
		t.Fatalf("message = %+v", m)
	}
}

func TestDecodeSmallResponses(t *testing.T) {
	cases := []struct {
		body  string
		check func(*testing.T, *Message)
	}{
		{
			`<cwmp:DeleteObjectResponse><cwmp:Status>1</cwmp:Status></cwmp:DeleteObjectResponse>`,
			func(t *testing.T, m *Message) {
				if m.Kind != KindDeleteObjectResponse || m.DeleteObjectResponse.Status != 1 {
					t.Fatalf("message = %+v", m)
				}
			},
		},
		{
			`<cwmp:RebootResponse/>`,
			func(t *testing.T, m *Message) {
				if m.Kind != KindRebootResponse {
					t.Fatalf("kind = %v", m.Kind)
				}
			},
		},
		{
			`<cwmp:FactoryResetResponse></cwmp:FactoryResetResponse>`,
			func(t *testing.T, m *Message) {
				if m.Kind != KindFactoryResetResponse {
					t.Fatalf("kind = %v", m.Kind)
				}
			},
		},
		{
			`<cwmp:DownloadResponse><cwmp:Status>1</cwmp:Status><cwmp:StartTime>2024-01-01T00:00:00Z</cwmp:StartTime><cwmp:CompleteTime>2024-01-01T00:01:00Z</cwmp:CompleteTime></cwmp:DownloadResponse>`,
			func(t *testing.T, m *Message) {
				d := m.DownloadResponse
				if m.Kind != KindDownloadResponse || d.Status != 1 || d.StartTime != "2024-01-01T00:00:00Z" || d.CompleteTime != "2024-01-01T00:01:00Z" {
					t.Fatalf("message = %+v", m)
				}
			},
		},
	}
	for _, tc := range cases {
		in := `<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0"><soap-env:Body>` + tc.body + `</soap-env:Body></soap-env:Envelope>`
		m, err := Decode([]byte(in))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tc.body, err)
		}
		tc.check(t, m)
	}
}

func TestDecodeIntoReusesDocAfterReuse(t *testing.T) {
	var d Doc
	m1 := &Message{}
	if err := DecodeInto(fixture(t, "inform_boot.xml"), m1, &d); err != nil {
		t.Fatal(err)
	}
	m2 := &Message{}
	if err := DecodeInto(fixture(t, "gpv_response_array.xml"), m2, &d); err != nil {
		t.Fatal(err)
	}
	// m1's strings must survive the Doc reuse (they are copies).
	if m1.Inform.DeviceID.Manufacturer != "Huawei Technologies Co., Ltd." {
		t.Fatalf("m1 corrupted: %+v", m1.Inform.DeviceID)
	}
	if len(m2.GetParameterValuesResponse.Params) != 200 {
		t.Fatalf("m2 params = %d", len(m2.GetParameterValuesResponse.Params))
	}
}
