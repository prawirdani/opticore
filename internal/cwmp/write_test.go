package cwmp

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"strings"
	"testing"
)

func goldenEnvelope(ver, id, body string) string {
	ns := map[string]string{
		"1.0": "urn:dslforum-org:cwmp-1-0",
		"1.1": "urn:dslforum-org:cwmp-1-1",
		"1.2": "urn:dslforum-org:cwmp-1-2",
		"1.3": "urn:dslforum-org:cwmp-1-2",
		"1.4": "urn:dslforum-org:cwmp-1-3",
	}[ver]
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<soap-env:Envelope xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="` + ns +
		`"><soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">` + id +
		`</cwmp:ID></soap-env:Header><soap-env:Body>` + body +
		`</soap-env:Body></soap-env:Envelope>`
}

func TestAppendGolden(t *testing.T) {
	spvParams := []ParamValue{
		{Name: "Device.WiFi.SSID.1.SSID", Value: "HOME", Type: "xsd:string"},
		{Name: "Device.IP.Interface.1.Enable", Value: "1", Type: "xsd:boolean"},
	}
	cases := []struct {
		name string
		got  []byte
		body string
	}{
		{
			"InformResponse",
			AppendInformResponse(nil, "1.0", "42"),
			`<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`,
		},
		{
			"GetRPCMethodsResponse",
			AppendGetRPCMethodsResponse(nil, "1.0", "42"),
			`<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[3]"><string>Inform</string><string>GetRPCMethods</string><string>TransferComplete</string></MethodList></cwmp:GetRPCMethodsResponse>`,
		},
		{
			"TransferCompleteResponse",
			AppendTransferCompleteResponse(nil, "1.0", "42"),
			`<cwmp:TransferCompleteResponse/>`,
		},
		{
			"GetParameterValues",
			AppendGetParameterValues(nil, "1.0", "42", []string{"a.b", "c.d"}),
			`<cwmp:GetParameterValues><ParameterNames soap-enc:arrayType="xsd:string[2]"><string>a.b</string><string>c.d</string></ParameterNames></cwmp:GetParameterValues>`,
		},
		{
			"GetParameterNames",
			AppendGetParameterNames(nil, "1.0", "42", "Device.WiFi.SSID.", true),
			`<cwmp:GetParameterNames><ParameterPath>Device.WiFi.SSID.</ParameterPath><NextLevel>1</NextLevel></cwmp:GetParameterNames>`,
		},
		{
			"SetParameterValues",
			AppendSetParameterValues(nil, "1.0", "42", spvParams, "task-1"),
			`<cwmp:SetParameterValues><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[2]"><ParameterValueStruct><Name>Device.WiFi.SSID.1.SSID</Name><Value xsi:type="xsd:string">HOME</Value></ParameterValueStruct><ParameterValueStruct><Name>Device.IP.Interface.1.Enable</Name><Value xsi:type="xsd:boolean">1</Value></ParameterValueStruct></ParameterList><ParameterKey>task-1</ParameterKey></cwmp:SetParameterValues>`,
		},
		{
			"AddObject",
			AppendAddObject(nil, "1.0", "42", "Device.WiFi.SSID.", "task-1"),
			`<cwmp:AddObject><ObjectName>Device.WiFi.SSID.</ObjectName><ParameterKey>task-1</ParameterKey></cwmp:AddObject>`,
		},
		{
			"DeleteObject",
			AppendDeleteObject(nil, "1.0", "42", "Device.WiFi.SSID.3.", "task-1"),
			`<cwmp:DeleteObject><ObjectName>Device.WiFi.SSID.3.</ObjectName><ParameterKey>task-1</ParameterKey></cwmp:DeleteObject>`,
		},
		{
			"Reboot",
			AppendReboot(nil, "1.0", "42", "task-1"),
			`<cwmp:Reboot><CommandKey>task-1</CommandKey></cwmp:Reboot>`,
		},
		{
			"FactoryReset",
			AppendFactoryReset(nil, "1.0", "42"),
			`<cwmp:FactoryReset/>`,
		},
		{
			"Download",
			AppendDownload(nil, "1.0", "42", "fw-1", "1 Firmware Upgrade Image", "http://acs.example/fw.bin", "u", "p", 123456789, "fw.bin", 30, "http://acs.example/ok", "http://acs.example/fail"),
			`<cwmp:Download><CommandKey>fw-1</CommandKey><FileType>1 Firmware Upgrade Image</FileType><URL>http://acs.example/fw.bin</URL><Username>u</Username><Password>p</Password><FileSize>123456789</FileSize><TargetFileName>fw.bin</TargetFileName><DelaySeconds>30</DelaySeconds><SuccessURL>http://acs.example/ok</SuccessURL><FailureURL>http://acs.example/fail</FailureURL></cwmp:Download>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := goldenEnvelope("1.0", "42", tc.body)
			if string(tc.got) != want {
				t.Fatalf("mismatch\n got: %s\nwant: %s", tc.got, want)
			}
			wellFormed(t, tc.got)
		})
	}
}

func TestEnvelopeNamespacesPerVersion(t *testing.T) {
	want := map[string]string{
		"1.0": "urn:dslforum-org:cwmp-1-0",
		"1.1": "urn:dslforum-org:cwmp-1-1",
		"1.2": "urn:dslforum-org:cwmp-1-2",
		"1.3": "urn:dslforum-org:cwmp-1-2",
		"1.4": "urn:dslforum-org:cwmp-1-3",
	}
	other := []string{
		`xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/"`,
		`xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/"`,
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema"`,
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"`,
	}
	for ver, ns := range want {
		out := string(AppendInformResponse(nil, ver, "1"))
		if !strings.Contains(out, `xmlns:cwmp="`+ns+`"`) {
			t.Fatalf("%s: missing cwmp namespace %q: %s", ver, ns, out)
		}
		for _, o := range other {
			if !strings.Contains(out, o) {
				t.Fatalf("%s: missing %s", ver, o)
			}
		}
		wellFormed(t, []byte(out))
	}
}

func TestAppendEscaping(t *testing.T) {
	params := []ParamValue{{Name: `A&B`, Value: `<x> "q" 'a'`, Type: `x"y`}}
	out := string(AppendSetParameterValues(nil, "1.0", `id&<>`, params, `k&k`))
	for _, want := range []string{
		`A&amp;B`,
		`&lt;x&gt; "q" 'a'`,
		`xsi:type="x&quot;y"`,
		`<cwmp:ID soap-env:mustUnderstand="1">id&amp;&lt;&gt;</cwmp:ID>`,
		`<ParameterKey>k&amp;k</ParameterKey>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	wellFormed(t, []byte(out))
}

func wellFormed(t *testing.T, b []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, b)
		}
	}
}

func parseOutput(t *testing.T, b []byte) (*Doc, int32) {
	t.Helper()
	d := &Doc{}
	if err := d.Parse(b); err != nil {
		t.Fatalf("Parse: %v\n%s", err, b)
	}
	body := d.Child(0, "Body")
	if body < 0 {
		t.Fatal("no Body")
	}
	method := d.Nodes[body].FirstChild
	if method < 0 {
		t.Fatal("no method")
	}
	return d, method
}

func childText(t *testing.T, d *Doc, i int32, local string) string {
	t.Helper()
	c := d.Child(i, local)
	if c < 0 {
		t.Fatalf("no child %q", local)
	}
	return string(d.Nodes[c].Text)
}

func TestRoundTripWriters(t *testing.T) {
	t.Run("InformResponse", func(t *testing.T) {
		out := AppendInformResponse(nil, "1.2", "55")
		d, m := parseOutput(t, out)
		if string(d.Nodes[m].Local) != "InformResponse" {
			t.Fatalf("method = %s", d.Nodes[m].Local)
		}
		if got := childText(t, d, m, "MaxEnvelopes"); got != "1" {
			t.Fatalf("MaxEnvelopes = %q", got)
		}
		if got := childText(t, d, d.Child(0, "Header"), "ID"); got != "55" {
			t.Fatalf("ID = %q", got)
		}
	})

	t.Run("GetRPCMethodsResponse", func(t *testing.T) {
		d, m := parseOutput(t, AppendGetRPCMethodsResponse(nil, "1.1", "9"))
		list := d.Child(m, "MethodList")
		if list < 0 {
			t.Fatal("no MethodList")
		}
		if at, ok := d.Attr(list, "arrayType"); !ok || string(at) != "xsd:string[3]" {
			t.Fatalf("arrayType = %q", at)
		}
		var got []string
		for c := d.Nodes[list].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
			got = append(got, string(d.Nodes[c].Text))
		}
		if strings.Join(got, ",") != "Inform,GetRPCMethods,TransferComplete" {
			t.Fatalf("methods = %v", got)
		}
	})

	t.Run("TransferCompleteResponse", func(t *testing.T) {
		d, m := parseOutput(t, AppendTransferCompleteResponse(nil, "1.0", "9"))
		if string(d.Nodes[m].Local) != "TransferCompleteResponse" || d.Nodes[m].FirstChild >= 0 {
			t.Fatalf("body = %+v", d.Nodes[m])
		}
	})

	t.Run("GetParameterValues", func(t *testing.T) {
		names := []string{"Device.WiFi.SSID.1.SSID", "Device.IP.Interface.1.Enable"}
		d, m := parseOutput(t, AppendGetParameterValues(nil, "1.0", "9", names))
		list := d.Child(m, "ParameterNames")
		if list < 0 {
			t.Fatal("no ParameterNames")
		}
		if at, _ := d.Attr(list, "arrayType"); string(at) != "xsd:string[2]" {
			t.Fatalf("arrayType = %q", at)
		}
		var got []string
		for c := d.Nodes[list].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
			got = append(got, string(d.Nodes[c].Text))
		}
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Fatalf("names = %v", got)
		}
	})

	t.Run("GetParameterNames", func(t *testing.T) {
		d, m := parseOutput(t, AppendGetParameterNames(nil, "1.0", "9", "Device.WiFi.SSID.", false))
		if got := childText(t, d, m, "ParameterPath"); got != "Device.WiFi.SSID." {
			t.Fatalf("path = %q", got)
		}
		if got := childText(t, d, m, "NextLevel"); got != "0" {
			t.Fatalf("nextLevel = %q", got)
		}
	})

	t.Run("SetParameterValues", func(t *testing.T) {
		params := []ParamValue{
			{Name: "A.B", Value: "v1", Type: "xsd:string"},
			{Name: "C.D", Value: "7", Type: "xsd:unsignedInt"},
			{Name: "E.F", Value: "x", Type: ""},
		}
		d, m := parseOutput(t, AppendSetParameterValues(nil, "1.3", "9", params, "task-7"))
		list := d.Child(m, "ParameterList")
		if list < 0 {
			t.Fatal("no ParameterList")
		}
		if at, _ := d.Attr(list, "arrayType"); string(at) != "cwmp:ParameterValueStruct[3]" {
			t.Fatalf("arrayType = %q", at)
		}
		n := 0
		for c := d.Nodes[list].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
			if string(d.Nodes[c].Local) != "ParameterValueStruct" {
				continue
			}
			want := params[n]
			if got := childText(t, d, c, "Name"); got != want.Name {
				t.Fatalf("param %d name = %q", n, got)
			}
			v := d.Child(c, "Value")
			if got := string(d.Nodes[v].Text); got != want.Value {
				t.Fatalf("param %d value = %q", n, got)
			}
			typ := want.Type
			if typ == "" {
				typ = "xsd:string"
			}
			if at, _ := d.Attr(v, "type"); string(at) != typ {
				t.Fatalf("param %d type = %q", n, at)
			}
			n++
		}
		if n != len(params) {
			t.Fatalf("structs = %d", n)
		}
		if got := childText(t, d, m, "ParameterKey"); got != "task-7" {
			t.Fatalf("key = %q", got)
		}
	})

	t.Run("AddObject", func(t *testing.T) {
		d, m := parseOutput(t, AppendAddObject(nil, "1.0", "9", "Device.WiFi.SSID.", "k"))
		if got := childText(t, d, m, "ObjectName"); got != "Device.WiFi.SSID." {
			t.Fatalf("object = %q", got)
		}
		if got := childText(t, d, m, "ParameterKey"); got != "k" {
			t.Fatalf("key = %q", got)
		}
	})

	t.Run("DeleteObject", func(t *testing.T) {
		d, m := parseOutput(t, AppendDeleteObject(nil, "1.0", "9", "Device.WiFi.SSID.3.", "k"))
		if got := childText(t, d, m, "ObjectName"); got != "Device.WiFi.SSID.3." {
			t.Fatalf("object = %q", got)
		}
		if got := childText(t, d, m, "ParameterKey"); got != "k" {
			t.Fatalf("key = %q", got)
		}
	})

	t.Run("Reboot", func(t *testing.T) {
		d, m := parseOutput(t, AppendReboot(nil, "1.4", "9", "cmd&1"))
		if got := childText(t, d, m, "CommandKey"); got != "cmd&1" {
			t.Fatalf("commandKey = %q", got)
		}
	})

	t.Run("FactoryReset", func(t *testing.T) {
		d, m := parseOutput(t, AppendFactoryReset(nil, "1.0", "9"))
		if string(d.Nodes[m].Local) != "FactoryReset" || d.Nodes[m].FirstChild >= 0 {
			t.Fatalf("body = %+v", d.Nodes[m])
		}
	})

	t.Run("Download", func(t *testing.T) {
		out := AppendDownload(nil, "1.2", "9",
			"fw-1", "1 Firmware Upgrade Image", "http://acs.example/fw.bin?a=1&b=2",
			"user", "pa&ss", 123456789, "fw.bin", 30,
			"http://acs.example/ok", "http://acs.example/fail")
		d, m := parseOutput(t, out)
		checks := map[string]string{
			"CommandKey":     "fw-1",
			"FileType":       "1 Firmware Upgrade Image",
			"URL":            "http://acs.example/fw.bin?a=1&b=2",
			"Username":       "user",
			"Password":       "pa&ss",
			"FileSize":       "123456789",
			"TargetFileName": "fw.bin",
			"DelaySeconds":   "30",
			"SuccessURL":     "http://acs.example/ok",
			"FailureURL":     "http://acs.example/fail",
		}
		for local, want := range checks {
			if got := childText(t, d, m, local); got != want {
				t.Fatalf("%s = %q, want %q", local, got, want)
			}
		}
	})
}

func BenchmarkParseInform(b *testing.B) {
	data, err := os.ReadFile("testdata/inform_boot.xml")
	if err != nil {
		b.Fatal(err)
	}
	var d Doc
	m := &Message{}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := DecodeInto(data, m, &d); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseGPV200(b *testing.B) {
	data, err := os.ReadFile("testdata/gpv_response_array.xml")
	if err != nil {
		b.Fatal(err)
	}
	var d Doc
	m := &Message{}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := DecodeInto(data, m, &d); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendSPV(b *testing.B) {
	params := make([]ParamValue, 10)
	for i := range params {
		params[i] = ParamValue{
			Name:  "Device.WiFi.AccessPoint.1.AssociatedDevice.1.SignalStrength",
			Value: "-55",
			Type:  "xsd:int",
		}
	}
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = AppendSetParameterValues(buf[:0], "1.2", "12345", params, "task-1")
	}
	_ = buf
}

type stdGPVDoc struct {
	Body struct {
		Resp struct {
			List struct {
				Params []struct {
					Name  string `xml:"Name"`
					Value struct {
						Type string `xml:"type,attr"`
						Text string `xml:",chardata"`
					} `xml:"Value"`
				} `xml:"ParameterValueStruct"`
			} `xml:"ParameterList"`
		} `xml:"GetParameterValuesResponse"`
	} `xml:"Body"`
}

func BenchmarkStdlibParseGPV200(b *testing.B) {
	data, err := os.ReadFile("testdata/gpv_response_array.xml")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var doc stdGPVDoc
		if err := xml.Unmarshal(data, &doc); err != nil {
			b.Fatal(err)
		}
	}
}
