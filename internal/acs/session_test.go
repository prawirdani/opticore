package acs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/prawirdani/opticore/client"
	"github.com/prawirdani/opticore/internal/config"
	"github.com/prawirdani/opticore/internal/cwmp"
	"github.com/prawirdani/opticore/internal/hook"
	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/wake"
)

var testDSN string

func TestMain(m *testing.M) {
	base := os.Getenv("OPTICORE_TEST_DSN")
	if base == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	schema := "test_acs_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acs tests: connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		fmt.Fprintf(os.Stderr, "acs tests: create schema: %v\n", err)
		os.Exit(1)
	}
	testDSN = strings.Replace(base, "?", "?search_path="+schema+"&", 1)
	if !strings.Contains(base, "?") {
		testDSN = base + "?search_path=" + schema
	}
	s, err := store.Open(ctx, testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acs tests: open: %v\n", err)
		os.Exit(1)
	}
	if err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "acs tests: migrate: %v\n", err)
		os.Exit(1)
	}
	s.Close()

	code := m.Run()
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil && code == 0 {
		code = 1
	}
	admin.Close(ctx)
	os.Exit(code)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	if testDSN == "" {
		t.Skip("set OPTICORE_TEST_DSN to run")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `TRUNCATE tasks, events, devices, jobs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// harness is a CWMP server plus a fake CPE that plays canned answers.
type harness struct {
	t    *testing.T
	acs  *httptest.Server
	svc  *Service
	st   *store.Store
	cpe  *http.Client
	sess *fakeCPE
}

type harnessOpt func(*harness, *Sessions)

func newHarness(t *testing.T, configure func(*fakeCPE)) *harness {
	return newHarnessWith(t, configure)
}

func newHarnessWith(t *testing.T, configure func(*fakeCPE), opts ...harnessOpt) *harness {
	t.Helper()
	st := testStore(t)
	sessions := &Sessions{Store: st,
		Logger: log.New(io.Discard, "", 0), GPVBatch: 32, MaxAttempts: 2}
	cs := &cwmp.Server{
		Handler: sessions,
		Logger:  log.New(io.Discard, "", 0),
		MaxBody: 10 << 20, SessionTimeout: 5 * time.Second, MaxSessions: 10,
	}
	cs.Start()
	t.Cleanup(cs.Stop)

	mux := http.NewServeMux()
	mux.Handle("/", cs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	f := &fakeCPE{t: t, url: srv.URL + "/", client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	if configure != nil {
		configure(f)
	}
	h := &harness{t: t, acs: srv, svc: &Service{Store: st}, st: st, cpe: f.client, sess: f}
	sessions.Svc = h.svc
	for _, o := range opts {
		o(h, sessions)
	}
	return h
}

// fakeCPE answers ACS requests from canned tables and records what it saw.
type fakeCPE struct {
	t      *testing.T
	url    string
	client *http.Client

	params   map[string]string // GPV answers
	faultSPV *string           // fault code to return on SPV instead of a response
	faultGPV *string
	root     string // data model root; "" means InternetGatewayDevice.

	mu       sync.Mutex
	sets     [][][3]string
	gpvs     [][]string
	rebooted bool
	deleted  []string
	added    int
	lastID   string
}

// taskSets returns the sets the ACS sent for tasks, excluding the ones that
// carry the connection-request credentials.
func (f *fakeCPE) taskSets() [][][3]string {
	var out [][][3]string
	for _, set := range f.sets {
		if strings.Contains(set[0][0], ".ManagementServer.ConnectionRequest") {
			continue
		}
		out = append(out, set)
	}
	return out
}

func (f *fakeCPE) post(body string) (int, []byte) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (f *fakeCPE) inform(serial string, events []string) int {
	return f.informWith(serial, events, informParams)
}

// informNoParams sends an Inform with an empty ParameterList, so the ACS
// cannot learn the device root.
func (f *fakeCPE) informNoParams(serial string, events []string) int {
	return f.informWith(serial, events, "")
}

func (f *fakeCPE) informWith(serial string, events []string, params string) int {
	f.t.Helper()
	body := fmt.Sprintf(informTpl, serial, eventList(events), time.Now().UTC().Format(time.RFC3339), params)
	if f.root != "" && f.root != "InternetGatewayDevice." {
		body = strings.ReplaceAll(body, "InternetGatewayDevice.", f.root)
	}
	code, _ := f.post(body)
	return code
}

func eventList(events []string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("<cwmp:EventStruct><cwmp:EventCode>" + e + "</cwmp:EventCode><cwmp:CommandKey></cwmp:CommandKey></cwmp:EventStruct>")
	}
	return b.String()
}

const informTpl = `<?xml version="1.0" encoding="UTF-8"?>
<soap-env:Envelope xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">inf</cwmp:ID></soap-env:Header>
<soap-env:Body><cwmp:Inform>
<cwmp:DeviceId><cwmp:Manufacturer>Huawei</cwmp:Manufacturer><cwmp:OUI>00259E</cwmp:OUI><cwmp:ProductClass>HG8145V5</cwmp:ProductClass><cwmp:SerialNumber>%s</cwmp:SerialNumber></cwmp:DeviceId>
<cwmp:EventList soap-enc:arrayType="cwmp:EventStruct[1]">%s</cwmp:EventList>
<cwmp:MaxEnvelopes>1</cwmp:MaxEnvelopes>
<cwmp:CurrentTime>%s</cwmp:CurrentTime>
<cwmp:RetryCount>0</cwmp:RetryCount>
%s
</cwmp:Inform></soap-env:Body></soap-env:Envelope>`

// informParams is the ParameterList of the default Inform: management server
// facts, the software version and a leaked CR password.
const informParams = `<cwmp:ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[4]">
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.ConnectionRequestURL</cwmp:Name><cwmp:Value>http://10.20.3.41:7547/abc</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.PeriodicInformInterval</cwmp:Name><cwmp:Value>300</cwmp:Value><cwmp:Type xsi:type="xsd:unsignedInt">xsd:unsignedInt</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.DeviceInfo.SoftwareVersion</cwmp:Name><cwmp:Value>V5R020C10S271</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.ConnectionRequestPassword</cwmp:Name><cwmp:Value>leaked</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
</cwmp:ParameterList>`

// run drives one device session to completion and returns how many ACS
// requests it answered. The loop mirrors CWMP: after the InformResponse the
// CPE POSTs an empty body, then answers each ACS request with a response.
func (f *fakeCPE) run() int {
	f.t.Helper()
	body := ""
	n := 0
	for {
		code, resp := f.post(body)
		if code == http.StatusNoContent {
			return n
		}
		if code != http.StatusOK {
			f.t.Fatalf("session step: status %d body %s", code, resp)
		}
		m, err := cwmp.DecodeACSRequest(resp)
		if err != nil {
			f.t.Fatalf("decode ACS request: %v (%s)", err, resp)
		}
		n++
		body = f.reply(m)
	}
}

// reply returns the CPE's answer to one ACS request.
func (f *fakeCPE) reply(m *cwmp.ACSRequest) string {
	f.t.Helper()
	id := m.ID
	switch m.Kind {
	case cwmp.ReqSetParameterValues:
		f.mu.Lock()
		var vals [][3]string
		for _, v := range m.Vals {
			vals = append(vals, [3]string{v.Name, v.Value, v.Type})
		}
		f.sets = append(f.sets, vals)
		f.mu.Unlock()
		if f.faultSPV != nil {
			return fault(faultTpl, id, *f.faultSPV)
		}
		return envelope(id, "<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>")
	case cwmp.ReqGetParameterValues:
		names := m.Names
		f.mu.Lock()
		f.gpvs = append(f.gpvs, names)
		f.mu.Unlock()
		if f.faultGPV != nil {
			return fault(faultTpl, id, *f.faultGPV)
		}
		var b strings.Builder
		count := 0
		for _, n := range names {
			for path, val := range f.params {
				if n == path || strings.HasSuffix(n, ".") && strings.HasPrefix(path, n) {
					fmt.Fprintf(&b, "<cwmp:ParameterValueStruct><cwmp:Name>%s</cwmp:Name><cwmp:Value>%s</cwmp:Value><cwmp:Type xsi:type=\"xsd:string\">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>", path, val)
					count++
				}
			}
		}
		return envelope(id, fmt.Sprintf(`<cwmp:GetParameterValuesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList></cwmp:GetParameterValuesResponse>`, count, b.String()))
	case cwmp.ReqAddObject:
		f.mu.Lock()
		f.added++
		inst := 2 + f.added
		f.mu.Unlock()
		return envelope(id, fmt.Sprintf(`<cwmp:AddObjectResponse><InstanceNumber>%d</InstanceNumber><Status>0</Status></cwmp:AddObjectResponse>`, inst))
	case cwmp.ReqDeleteObject:
		f.mu.Lock()
		f.deleted = append(f.deleted, m.Path)
		f.mu.Unlock()
		return envelope(id, "<cwmp:DeleteObjectResponse><Status>0</Status></cwmp:DeleteObjectResponse>")
	case cwmp.ReqReboot:
		f.mu.Lock()
		f.rebooted = true
		f.mu.Unlock()
		return envelope(id, "<cwmp:RebootResponse/>")
	case cwmp.ReqFactoryReset:
		return envelope(id, "<cwmp:FactoryResetResponse/>")
	}
	f.t.Fatalf("fake CPE: unhandled ACS request %d", m.Kind)
	return ""
}

const envelopeTpl = `<?xml version="1.0" encoding="UTF-8"?>
<soap-env:Envelope xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<soap-env:Header><cwmp:ID>%s</cwmp:ID></soap-env:Header><soap-env:Body>%s</soap-env:Body></soap-env:Envelope>`

const faultTpl = `<?xml version="1.0" encoding="UTF-8"?>
<soap-env:Envelope xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<soap-env:Header><cwmp:ID>%s</cwmp:ID></soap-env:Header><soap-env:Body>
<soap-env:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring><detail>
<cwmp:Fault><FaultCode>%s</FaultCode><FaultString>test fault</FaultString></cwmp:Fault>
</detail></soap-env:Fault></soap-env:Body></soap-env:Envelope>`

func envelope(id, body string) string   { return fmt.Sprintf(envelopeTpl, id, body) }
func fault(tpl, id, code string) string { return fmt.Sprintf(tpl, id, code) }

// ---- tests ----

func TestBootstrapSetsConnectionRequestCredentials(t *testing.T) {
	h := newHarness(t, nil)
	if code := h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"}); code != http.StatusOK {
		t.Fatalf("inform: %d", code)
	}
	if n := h.sess.run(); n != 1 {
		t.Fatalf("answered %d ACS requests, want 1 (CR credentials)", n)
	}
	d, err := h.st.GetDevice(context.Background(), "48575443BD7589AB")
	if err != nil {
		t.Fatal(err)
	}
	if d.CRSetAt == nil || d.CRUsername == "" || d.CRPassword == "" {
		t.Fatalf("CR credentials not stored: %+v", d)
	}
	if p := d.Params["InternetGatewayDevice.ManagementServer.ConnectionRequestPassword"]; p.Value != "leaked" {
		t.Errorf("inform password cached as %+v, want the raw value", p)
	}
	if d.InformInterval != 300 || d.SoftwareVersion != "V5R020C10S271" || d.LastAddr == "" {
		t.Errorf("device facts = %+v", d)
	}
	// Second session, no bootstrap: no CR set again.
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 0 {
		t.Errorf("periodic session sent %d requests, want 0", n)
	}
}

func TestSetTaskReadsPrevThenSetsAndCaches(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run() // CR credentials

	h.sess.params = map[string]string{
		"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID": "OldNet",
	}
	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID","NewNet","xsd:string"]]}`),
	}}); err != nil {
		t.Fatal(err)
	}

	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 2 {
		t.Fatalf("answered %d requests, want 2 (GPV prev, SPV)", n)
	}

	d, _ := h.st.GetDevice(ctx, "48575443BD7589AB")
	if got := d.Params["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"].Value; got != "NewNet" {
		t.Errorf("cached SSID = %q, want NewNet", got)
	}
	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if len(ts) != 1 || ts[0].Status != "done" {
		t.Fatalf("tasks = %+v", ts)
	}
	if !strings.Contains(string(ts[0].Prev), "OldNet") {
		t.Errorf("prev = %s, want the old SSID", ts[0].Prev)
	}
	if sets := h.sess.taskSets(); len(sets) != 1 || sets[0][0][1] != "NewNet" {
		t.Errorf("device saw sets %v", sets)
	}
}

func TestSecretSetStoredRawAndSentPlaintext(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","u"],["InternetGatewayDevice.ManagementServer.Password","hunter2","xsd:string"]]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if !strings.Contains(string(ts[0].Args), "hunter2") {
		t.Errorf("args not stored as written: %s", ts[0].Args)
	}
	if strings.Contains(string(ts[0].Args), "enc:") {
		t.Errorf("args still sealed: %s", ts[0].Args)
	}
	h.sess.params = map[string]string{}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	if sets := h.sess.taskSets(); len(sets) != 1 || sets[0][1][1] != "hunter2" {
		t.Fatalf("device received %v, want the plaintext password", sets)
	}
}

func TestRebootAndBatchOrder(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{
		{Name: "add_object", Args: json.RawMessage(`{"path":"InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection."}`)},
		{Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.{instance}.Username","s0004@isp","xsd:string"]]}`)},
	}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	sets := h.sess.taskSets()
	if len(sets) != 1 {
		t.Fatalf("sets = %v", sets)
	}
	if got := sets[0][0][0]; !strings.Contains(got, "WANPPPConnection.3.Username") {
		t.Errorf("set path = %q, want {instance} replaced with 3", got)
	}
	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	for _, x := range ts {
		if x.Status != "done" {
			t.Errorf("task %d %s = %s, want done", x.ID, x.Name, x.Status)
		}
	}

	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{{Name: "reboot"}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	if !h.sess.rebooted {
		t.Error("device was not rebooted")
	}
}

func TestPermanentFaultFailsTaskAndCancelsBatch(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	faultCode := "9003"
	h.sess.faultSPV = &faultCode
	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{
		{Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","u","xsd:string"]]}`)},
		{Name: "reboot"},
	}); err != nil {
		t.Fatal(err)
	}
	h.sess.params = map[string]string{}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()

	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	var failed, cancelled int
	for _, x := range ts {
		switch x.Status {
		case "failed":
			failed++
			if x.FaultCode == nil || *x.FaultCode != 9003 {
				t.Errorf("fault code = %v, want 9003", x.FaultCode)
			}
		case "cancelled":
			cancelled++
		}
	}
	if failed != 1 || cancelled != 1 {
		t.Fatalf("failed=%d cancelled=%d, want 1 and 1: %+v", failed, cancelled, ts)
	}
	d, _ := h.st.GetDevice(ctx, "48575443BD7589AB")
	if !strings.Contains(string(d.LastFault), "9003") {
		t.Errorf("device last_fault = %s", d.LastFault)
	}
	if h.sess.rebooted {
		t.Error("reboot ran after the batch failed")
	}
}

func TestRetryableFaultRequeuesTask(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	code9002 := "9002"
	h.sess.faultSPV = &code9002
	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{
		{Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","u","xsd:string"]]}`)},
	}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()

	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if ts[0].Status != "pending" || ts[0].Attempts != 1 {
		t.Fatalf("task = %s attempts %d, want pending with 1 attempt", ts[0].Status, ts[0].Attempts)
	}
	// Second attempt succeeds.
	h.sess.faultSPV = nil
	h.sess.params = map[string]string{}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	ts, _ = h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if ts[0].Status != "done" {
		t.Fatalf("task = %s, want done", ts[0].Status)
	}
}

func TestReservedCPathRejected(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.svc.Enqueue(context.Background(), "no-such-device", []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.ConnectionRequestPassword","x"]]}`),
	}})
	if err == nil || !strings.Contains(err.Error(), "managed by the ACS") {
		t.Fatalf("err = %v, want reserved-path rejection", err)
	}
}

// A device that restarts its session (lost InformResponse, power cycle) must
// not be locked out: an idle session is replaced by the new Inform.
func TestIdleSessionReplacedByNewInform(t *testing.T) {
	h := newHarness(t, nil)
	if code := h.sess.inform("48575443BD7589AB", []string{"1 BOOT"}); code != http.StatusOK {
		t.Fatal(code)
	}
	// The first session is abandoned without answering the queued CR set.
	code, body := h.sess.post(fmt.Sprintf(informTpl, "48575443BD7589AB", eventList([]string{"1 BOOT"}), time.Now().UTC().Format(time.RFC3339), informParams))
	if code != http.StatusOK {
		t.Fatalf("second inform: %d %s", code, body)
	}
	if n := h.sess.run(); n != 1 {
		t.Fatalf("new session answered %d requests, want 1", n)
	}
	d, _ := h.st.GetDevice(context.Background(), "48575443BD7589AB")
	if d.CRSetAt == nil {
		t.Error("credentials were not set after the replacement session")
	}
}

func TestDeviceIDMatchingGenieACS(t *testing.T) {
	got := cwmp.DeviceIDOf(cwmp.DeviceID{OUI: "00259E", ProductClass: "HG8145V5", SerialNumber: "48575443BD7589AB"})
	if want := "00259E-HG8145V5-48575443BD7589AB"; got != want {
		t.Errorf("id = %q, want %q", got, want)
	}
	got = cwmp.DeviceIDOf(cwmp.DeviceID{OUI: "0025-9E", ProductClass: "HG 8145/V5", SerialNumber: "A:B"})
	if want := "0025%2D9E-HG%208145%2FV5-A%3AB"; got != want {
		t.Errorf("escaped id = %q, want %q (GenieACS does not zero-pad)", got, want)
	}
}

func TestStartupConfigValidation(t *testing.T) {
	if _, err := config.Load(func(string) string { return "" }); err == nil {
		t.Fatal("empty environment accepted")
	}
}

// A session that dies after a non-idempotent RPC was sent must not be retried:
// the device may already have rebooted (README §5).
func TestCloseMarksNonIdempotentInflightUnknown(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	_, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{
		{Name: "reboot"},
		{Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","u","xsd:string"]]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	code, resp := h.sess.post("")
	if code != http.StatusOK {
		t.Fatalf("post: %d", code)
	}
	m, err := cwmp.DecodeACSRequest(resp)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != cwmp.ReqReboot {
		t.Fatalf("first RPC = %d, want Reboot", m.Kind)
	}
	// Drop the connection: no answer. The ACME server ends the session when the
	// device's next Inform replaces it.
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})

	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	var unknown, cancelled, done int
	for _, x := range ts {
		switch x.Status {
		case "unknown":
			unknown++
		case "cancelled":
			cancelled++
		case "done":
			done++
		}
	}
	if unknown != 1 || cancelled != 1 {
		t.Fatalf("unknown=%d cancelled=%d done=%d, want 1/1/0: %+v", unknown, cancelled, done, ts)
	}
}

// A provisioner runs inside the bootstrapping session: prefetch, step 1 with
// tasks, execution, step 2 with results, then the session ends. No wake, no
// second session.
func TestHookProvisionerRunsInSession(t *testing.T) {
	var steps []hook.Step
	prov := httptest.NewServer(client.HookHandler("k", func(_ context.Context, s hook.Step) ([]hook.Task, error) {
		steps = append(steps, s)
		switch s.Step {
		case 1:
			return []hook.Task{
				{Name: "add_object", Args: json.RawMessage(`{"path":"InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection."}`)},
				{Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.{instance}.Username","s0004@isp","xsd:string"]]}`)},
			}, nil
		default:
			return nil, nil
		}
	}))
	defer prov.Close()

	h := newHarnessWith(t, func(f *fakeCPE) {
		f.params = map[string]string{"InternetGatewayDevice.ManagementServer.PeriodicInformInterval": "300"}
	}, func(_ *harness, s *Sessions) {
		c, err := hook.New(prov.URL, "k", 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		s.Hook = c
		s.HookEvents = []string{"0 BOOTSTRAP"}
		s.HookPrefetch = []string{"InternetGatewayDevice.ManagementServer.PeriodicInformInterval"}
		s.HookTimeout = 10 * time.Second
	})
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	if n := h.sess.run(); n != 5 { // CR set, prefetch, add_object, GPV prev, SPV
		t.Fatalf("answered %d ACS requests, want 5", n)
	}

	if len(steps) != 2 {
		t.Fatalf("provisioner saw %d steps, want 2", len(steps))
	}
	if steps[0].Device.Serial != "48575443BD7589AB" || steps[0].Device.ProductClass != "HG8145V5" {
		t.Errorf("step 1 device = %+v", steps[0].Device)
	}
	if steps[0].Prefetch["InternetGatewayDevice.ManagementServer.PeriodicInformInterval"].Value != "300" {
		t.Errorf("prefetch = %+v", steps[0].Prefetch)
	}
	if len(steps[1].Results) != 2 || steps[1].Results[0].Status != "done" {
		t.Fatalf("step 2 results = %+v", steps[1].Results)
	}
	if got := string(steps[1].Results[0].Result); !strings.Contains(got, "instance") {
		t.Errorf("add_object result = %s", got)
	}
	// The set used the instance number from add_object.
	sets := h.sess.taskSets()
	if len(sets) != 1 || !strings.Contains(sets[0][0][0], "WANPPPConnection.3.Username") {
		t.Fatalf("sets = %v", sets)
	}
	ts, _ := h.st.ListTasks(context.Background(), "48575443BD7589AB", 10)
	for _, x := range ts {
		if x.Status != "done" {
			t.Errorf("task %d %s = %s (%s)", x.ID, x.Name, x.Status, x.Fault)
		}
	}
}

// A hook that fails must not break the session: CR credentials still apply,
// queued API tasks still run, and the inform is stored.
func TestHookFailureFallsBack(t *testing.T) {
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer prov.Close()

	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) {
		c, err := hook.New(prov.URL, "k", 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		s.Hook = c
		s.HookEvents = []string{"0 BOOTSTRAP"}
	})
	// Bootstrap: the hook fails, CR credentials and the session still work.
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	if n := h.sess.run(); n != 1 { // CR set only
		t.Fatalf("answered %d ACS requests, want 1", n)
	}
	// Queued API tasks still run on the next session.
	if _, err := h.svc.Enqueue(context.Background(), "48575443BD7589AB", []store.NewTask{{
		Name: "get", Args: json.RawMessage(`{"paths":["InternetGatewayDevice.DeviceInfo.UpTime"]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.params = map[string]string{"InternetGatewayDevice.DeviceInfo.UpTime": "42"}
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	ts, _ := h.st.ListTasks(context.Background(), "48575443BD7589AB", 10)
	if len(ts) != 1 || ts[0].Status != "done" {
		t.Fatalf("queued task after a failed hook = %+v", ts)
	}

	d, err := h.st.GetDevice(context.Background(), "48575443BD7589AB")
	if err != nil {
		t.Fatal(err)
	}
	if d.CRSetAt == nil {
		t.Error("CR credentials were not set after the hook failed")
	}
}

// Only events on the trigger list start the hook.
func TestHookNotCalledWithoutTriggerEvent(t *testing.T) {
	calls := 0
	prov := httptest.NewServer(client.HookHandler("k", func(context.Context, hook.Step) ([]hook.Task, error) {
		calls++
		return nil, nil
	}))
	defer prov.Close()

	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) {
		c, _ := hook.New(prov.URL, "k", time.Second)
		s.Hook = c
		s.HookEvents = []string{"0 BOOTSTRAP"}
	})
	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	h.sess.run()
	if calls != 0 {
		t.Errorf("provisioner called %d times on a periodic inform", calls)
	}
}

// A device that refuses our connection-request credentials (factory reset, or a
// reboot that restored older settings) must not stay unreachable: the stored
// credentials are dropped so the next Inform sets them again.
func TestWakeUnauthorizedClearsCredentials(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP"})
	h.sess.run()

	d, err := h.st.GetDevice(ctx, "48575443BD7589AB")
	if err != nil || d.CRSetAt == nil {
		t.Fatalf("setup: %+v %v", d, err)
	}
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Digest realm="x", nonce="n", qop="auth", algorithm="MD5"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer deny.Close()

	if _, err := h.st.Pool().Exec(ctx, `UPDATE devices SET cr_url = $2 WHERE id = $1`, d.ID, deny.URL); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: h.st, Waker: wake.New(time.Second, log.New(io.Discard, "", 0))}
	if err := svc.Wake(ctx, "48575443BD7589AB"); !errors.Is(err, wake.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	d, _ = h.st.GetDevice(ctx, "48575443BD7589AB")
	if d.CRSetAt != nil || d.CRUsername != "" || d.CRPassword != "" {
		t.Errorf("credentials kept after a refused wake: %+v", d)
	}
}

// A ConnectionRequestURL that does not point back at the Inform's source is
// not stored and earns no credentials: a spoofed Inform cannot redirect wakes.
func TestCRURLMustMatchSource(t *testing.T) {
	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) { s.CRMatchSource = true })
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP"}) // URL host 10.20.3.41, source 127.0.0.1
	if n := h.sess.run(); n != 0 {
		t.Fatalf("answered %d ACS requests, want 0 (no credential write)", n)
	}
	d, err := h.st.GetDevice(context.Background(), "48575443BD7589AB")
	if err != nil {
		t.Fatal(err)
	}
	if d.CRURL != "" || d.CRSetAt != nil {
		t.Errorf("unverified CR URL kept: url=%q set=%v", d.CRURL, d.CRSetAt)
	}
}

func TestCRHostIs(t *testing.T) {
	for _, tc := range []struct {
		url, addr string
		want      bool
	}{
		{"http://172.16.106.10:7547/x", "172.16.106.10", true},
		{"http://172.16.106.10:7547/x", "::ffff:172.16.106.10", true},
		{"http://172.16.106.11:7547/x", "172.16.106.10", false},
		{"http://evil.example/x", "172.16.106.10", false},
		{"::", "172.16.106.10", false},
	} {
		if got := crHostIs(tc.url, tc.addr); got != tc.want {
			t.Errorf("crHostIs(%q,%q) = %v", tc.url, tc.addr, got)
		}
	}
}

// gpvNames returns the parameter name lists sent in GetParameterValues.
func (f *fakeCPE) gpvNames() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.gpvs...)
}

func TestGetTaskWithoutPathsReadsTheDeviceRoot(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run() // CR credentials

	h.sess.params = map[string]string{
		"InternetGatewayDevice.DeviceInfo.SoftwareVersion":           "V5R020C10S271",
		"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID": "HomeNet",
	}
	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{{Name: "get"}}); err != nil {
		t.Fatal(err)
	}

	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 1 {
		t.Fatalf("answered %d requests, want 1 (one GPV of the root)", n)
	}
	gpvs := h.sess.gpvNames()
	if len(gpvs) != 1 || len(gpvs[0]) != 1 || gpvs[0][0] != "InternetGatewayDevice." {
		t.Fatalf("device saw GPV names %v, want one partial root path", gpvs)
	}
	d, _ := h.st.GetDevice(ctx, "48575443BD7589AB")
	if got := d.Params["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"].Value; got != "HomeNet" {
		t.Errorf("cached SSID = %q, want HomeNet", got)
	}
	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if len(ts) != 1 || ts[0].Status != "done" {
		t.Fatalf("tasks = %+v", ts)
	}
	var res struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(ts[0].Result, &res); err != nil || res.Count != 2 {
		t.Errorf("result = %s (err %v), want count 2", ts[0].Result, err)
	}
}

func TestGetTaskCountsEveryChunk(t *testing.T) {
	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) { s.GPVBatch = 1 })
	ctx := context.Background()
	h.sess.inform("48575443BD7589AB", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run() // CR credentials

	h.sess.params = map[string]string{
		"InternetGatewayDevice.DeviceInfo.SoftwareVersion": "V5R020C10S271",
		"InternetGatewayDevice.DeviceInfo.SerialNumber":    "48575443BD7589AB",
	}
	if _, err := h.svc.Enqueue(ctx, "48575443BD7589AB", []store.NewTask{{
		Name: "get", Args: json.RawMessage(`{"paths":["InternetGatewayDevice.DeviceInfo.SoftwareVersion","InternetGatewayDevice.DeviceInfo.SerialNumber"]}`),
	}}); err != nil {
		t.Fatal(err)
	}

	h.sess.inform("48575443BD7589AB", []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 2 {
		t.Fatalf("answered %d requests, want 2 (one GPV per name at batch 1)", n)
	}
	ts, _ := h.st.ListTasks(ctx, "48575443BD7589AB", 10)
	if len(ts) != 1 || ts[0].Status != "done" {
		t.Fatalf("tasks = %+v", ts)
	}
	var res struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(ts[0].Result, &res); err != nil || res.Count != 2 {
		t.Errorf("result = %s (err %v), want count 2 across both chunks", ts[0].Result, err)
	}
}

// ---- automatic tree read ----

func withAutoTreeRead(_ *harness, s *Sessions) { s.AutoTreeRead = true }

func countTasks(t *testing.T, h *harness, serial, name string) int {
	t.Helper()
	ts, err := h.st.ListTasks(context.Background(), serial, 20)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range ts {
		if x.Name == name {
			n++
		}
	}
	return n
}

// A device that has never had its tree read gets one root get at the end of
// its first session, and never a second one while the cache stays whole.
func TestAutoTreeReadFillsCacheOnFirstInform(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h := newHarnessWith(t, func(f *fakeCPE) {
		f.params = map[string]string{
			"InternetGatewayDevice.DeviceInfo.SoftwareVersion":           "V5R020C10S271",
			"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID": "HomeNet",
		}
	}, withAutoTreeRead)

	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	if n := h.sess.run(); n != 2 { // CR credentials, then the tree read
		t.Fatalf("answered %d requests, want 2 (CR set, root GPV)", n)
	}
	gpvs := h.sess.gpvNames()
	if len(gpvs) != 1 || len(gpvs[0]) != 1 || gpvs[0][0] != "InternetGatewayDevice." {
		t.Fatalf("GPVs = %v, want one root partial path", gpvs)
	}
	d, err := h.st.GetDevice(ctx, serial)
	if err != nil {
		t.Fatal(err)
	}
	if d.TreeReadAt == nil {
		t.Error("tree_read_at not set after the root read")
	}
	if got := d.Params["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"].Value; got != "HomeNet" {
		t.Errorf("cached SSID = %q, want HomeNet", got)
	}
	ts, _ := h.st.ListTasks(ctx, serial, 20)
	get := findTask(t, ts, "get")
	if get.Status != "done" {
		t.Errorf("auto read task = %s, want done", get.Status)
	}

	// Second inform: the tree is cached, so nothing is queued.
	h.sess.inform(serial, []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 0 {
		t.Fatalf("second session answered %d requests, want 0", n)
	}
	if n := countTasks(t, h, serial, "get"); n != 1 {
		t.Errorf("get tasks = %d, want 1", n)
	}
}

// 0 BOOTSTRAP replaces the cache, so the tree must be read again.
func TestAutoTreeReadBootstrapReadsAgain(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h := newHarnessWith(t, nil, withAutoTreeRead)

	h.sess.inform(serial, []string{"1 BOOT"})
	h.sess.run()
	d, _ := h.st.GetDevice(ctx, serial)
	if d.TreeReadAt == nil {
		t.Fatal("tree_read_at not set after the first session")
	}

	h.sess.inform(serial, []string{"0 BOOTSTRAP"})
	if n := h.sess.run(); n != 2 { // CR credentials, root GPV
		t.Fatalf("bootstrap session answered %d requests, want 2", n)
	}
	d, _ = h.st.GetDevice(ctx, serial)
	if d.TreeReadAt == nil {
		t.Error("tree_read_at not set again after the bootstrap")
	}
	if n := countTasks(t, h, serial, "get"); n != 2 {
		t.Errorf("get tasks = %d, want 2", n)
	}
}

// A root get already queued (by the API) must not be duplicated, even when it
// is not runnable yet.
func TestAutoTreeReadSkipsPendingRootGet(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	var sessions *Sessions
	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) { sessions = s })

	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run() // CR credentials only: auto read off here

	if _, err := h.st.CreateTasks(ctx, serial, []store.NewTask{
		{Name: "get", RunAt: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	sessions.AutoTreeRead = true
	h.sess.inform(serial, []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 0 {
		t.Fatalf("answered %d requests, want 0", n)
	}
	if n := countTasks(t, h, serial, "get"); n != 1 {
		t.Errorf("get tasks = %d, want 1 (the queued operator get)", n)
	}
}

// The automatic read runs after the connection-request credentials and after
// the tasks already queued.
func TestAutoTreeReadRunsLast(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h := newHarnessWith(t, func(f *fakeCPE) {
		f.params = map[string]string{"InternetGatewayDevice.DeviceInfo.UpTime": "42"}
	}, withAutoTreeRead)

	h.sess.inform(serial, []string{"1 BOOT"})
	if _, err := h.svc.CreateTasks(ctx, serial, []store.NewTask{{
		Name: "get", Args: json.RawMessage(`{"paths":["InternetGatewayDevice.DeviceInfo.UpTime"]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if n := h.sess.run(); n != 3 { // CR set, operator get, tree read
		t.Fatalf("answered %d requests, want 3", n)
	}
	if sets := h.sess.sets; len(sets) == 0 ||
		!strings.Contains(sets[0][0][0], ".ManagementServer.ConnectionRequest") {
		t.Fatalf("sets = %v, want the CR credential SPV first", sets)
	}
	gpvs := h.sess.gpvNames()
	if len(gpvs) != 2 {
		t.Fatalf("GPVs = %v, want the operator get then the tree read", gpvs)
	}
	if len(gpvs[0]) != 1 || gpvs[0][0] != "InternetGatewayDevice.DeviceInfo.UpTime" {
		t.Errorf("first GPV = %v, want the operator path", gpvs[0])
	}
	if len(gpvs[1]) != 1 || gpvs[1][0] != "InternetGatewayDevice." {
		t.Errorf("last GPV = %v, want the root partial path", gpvs[1])
	}
}

func TestAutoTreeReadDisabled(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h := newHarness(t, nil) // Sessions.AutoTreeRead off

	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	if n := h.sess.run(); n != 1 { // CR credentials only
		t.Fatalf("answered %d requests, want 1", n)
	}
	if n := len(h.sess.gpvNames()); n != 0 {
		t.Errorf("GPVs = %d, want 0", n)
	}
	if n := countTasks(t, h, serial, "get"); n != 0 {
		t.Errorf("get tasks = %d, want 0", n)
	}
	d, _ := h.st.GetDevice(ctx, serial)
	if d.TreeReadAt != nil {
		t.Error("tree_read_at set while auto read is disabled")
	}
}

// Without a root from the Inform there is nothing to read.
func TestAutoTreeReadNeedsRoot(t *testing.T) {
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h := newHarnessWith(t, nil, withAutoTreeRead)

	h.sess.informNoParams(serial, []string{"1 BOOT"})
	if n := h.sess.run(); n != 0 {
		t.Fatalf("answered %d requests, want 0", n)
	}
	if n := countTasks(t, h, serial, "get"); n != 0 {
		t.Errorf("get tasks = %d, want 0", n)
	}
	d, _ := h.st.GetDevice(ctx, serial)
	if d.TreeReadAt != nil {
		t.Error("tree_read_at set without a known root")
	}
}

// ---- ping ----

func findTask(t *testing.T, ts []store.Task, name string) store.Task {
	t.Helper()
	for _, x := range ts {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("no %s task in %+v", name, ts)
	return store.Task{}
}

func TestPingTwoSessionFlow(t *testing.T) {
	cases := []struct {
		name string
		root string
		obj  string
	}{
		{"TR-098", "", "InternetGatewayDevice.IPPingDiagnostics."},
		{"TR-181", "Device.", "Device.IP.Diagnostics.IPPing."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(f *fakeCPE) { f.root = tc.root })
			ctx := context.Background()
			serial := "48575443BD7589AB"
			h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
			h.sess.run() // CR credentials

			if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
				Name: "ping", Args: json.RawMessage(`{"host":"8.8.8.8"}`),
			}}); err != nil {
				t.Fatal(err)
			}

			// Session 1: only the request SPV.
			h.sess.inform(serial, []string{"2 PERIODIC"})
			if n := h.sess.run(); n != 1 {
				t.Fatalf("answered %d requests, want 1 (ping SPV)", n)
			}
			sets := h.sess.taskSets()
			if len(sets) != 1 {
				t.Fatalf("sets = %v, want one ping SPV", sets)
			}
			want := map[string]string{
				tc.obj + "DiagnosticsState":    "Requested",
				tc.obj + "Host":                "8.8.8.8",
				tc.obj + "NumberOfRepetitions": "4",
				tc.obj + "Timeout":             "1000",
				tc.obj + "DataBlockSize":       "64",
			}
			got := map[string]string{}
			for _, v := range sets[0] {
				got[v[0]] = v[1]
			}
			for name, val := range want {
				if got[name] != val {
					t.Errorf("SPV %s = %q, want %q (all %v)", name, got[name], val, got)
				}
			}
			if len(got) != len(want) {
				t.Errorf("SPV leaves = %v, want %d leaves", got, len(want))
			}
			ts, _ := h.st.ListTasks(ctx, serial, 10)
			ping := findTask(t, ts, "ping")
			if ping.Status != "running" || !strings.Contains(string(ping.Result), `"requested"`) {
				t.Fatalf("ping after SPV = %s %s, want running/requested", ping.Status, ping.Result)
			}

			// Session 2: event 8, one GPV of the result leaves.
			h.sess.params = map[string]string{
				tc.obj + "DiagnosticsState":    "Complete",
				tc.obj + "SuccessCount":        "4",
				tc.obj + "FailureCount":        "0",
				tc.obj + "AverageResponseTime": "3",
				tc.obj + "MinimumResponseTime": "2",
				tc.obj + "MaximumResponseTime": "5",
			}
			h.sess.inform(serial, []string{"8 DIAGNOSTICS COMPLETE"})
			if n := h.sess.run(); n != 1 {
				t.Fatalf("answered %d requests, want 1 (result GPV)", n)
			}
			gpvs := h.sess.gpvNames()
			if len(gpvs) != 1 {
				t.Fatalf("GPVs = %v, want one", gpvs)
			}
			wantNames := pingResultNames(tc.obj)
			if len(gpvs[0]) != len(wantNames) {
				t.Fatalf("result GPV names = %v, want %v", gpvs[0], wantNames)
			}
			for i, name := range wantNames {
				if gpvs[0][i] != name {
					t.Errorf("result GPV name %d = %q, want %q", i, gpvs[0][i], name)
				}
			}
			ts, _ = h.st.ListTasks(ctx, serial, 10)
			ping = findTask(t, ts, "ping")
			if ping.Status != "done" {
				t.Fatalf("ping = %s %s, want done", ping.Status, ping.Result)
			}
			var res struct {
				State   string `json:"state"`
				Success int    `json:"success"`
				Failure int    `json:"failure"`
				Avg     int    `json:"avg_ms"`
				Min     int    `json:"min_ms"`
				Max     int    `json:"max_ms"`
			}
			if err := json.Unmarshal(ping.Result, &res); err != nil {
				t.Fatal(err)
			}
			if res.State != "Complete" || res.Success != 4 || res.Failure != 0 ||
				res.Avg != 3 || res.Min != 2 || res.Max != 5 {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestPingNonCompleteStateFailsWithNumbersKept(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	obj := "InternetGatewayDevice.IPPingDiagnostics."
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "ping", Args: json.RawMessage(`{"host":"bad.example"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run()

	h.sess.params = map[string]string{
		obj + "DiagnosticsState":    "Error_CannotResolveHostName",
		obj + "SuccessCount":        "0",
		obj + "FailureCount":        "4",
		obj + "AverageResponseTime": "0",
		obj + "MinimumResponseTime": "0",
		obj + "MaximumResponseTime": "0",
	}
	h.sess.inform(serial, []string{"8 DIAGNOSTICS COMPLETE"})
	h.sess.run()

	ts, _ := h.st.ListTasks(ctx, serial, 10)
	ping := findTask(t, ts, "ping")
	if ping.Status != "failed" || ping.Fault != "Error_CannotResolveHostName" {
		t.Fatalf("ping = %s fault %q, want failed/Error_CannotResolveHostName", ping.Status, ping.Fault)
	}
	if !strings.Contains(string(ping.Result), `"failure"`) {
		t.Errorf("result = %s, want the parsed counts kept", ping.Result)
	}
	var res struct {
		Failure int `json:"failure"`
	}
	if err := json.Unmarshal(ping.Result, &res); err != nil || res.Failure != 4 {
		t.Errorf("result = %s (err %v), want failure 4 kept", ping.Result, err)
	}
}

func TestPingWaitsWithoutEvent8AndOtherTasksRun(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "ping", Args: json.RawMessage(`{"host":"8.8.8.8"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run() // SPV, now waiting

	// A second ping stays pending while the first waits.
	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{Name: "ping", Args: json.RawMessage(`{"host":"1.1.1.1"}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{Name: "reboot"}}); err != nil {
		t.Fatal(err)
	}

	h.sess.inform(serial, []string{"2 PERIODIC"})
	if n := h.sess.run(); n != 1 {
		t.Fatalf("answered %d requests, want 1 (reboot only)", n)
	}
	if !h.sess.rebooted {
		t.Error("device was not rebooted while the ping waited")
	}
	ts, _ := h.st.ListTasks(ctx, serial, 10)
	for _, x := range ts {
		switch x.Name {
		case "ping":
			if x.Attempts == 1 && x.Status != "running" {
				t.Errorf("first ping = %s, want still running", x.Status)
			}
			if x.Attempts == 0 && x.Status != "pending" {
				t.Errorf("second ping = %s, want pending", x.Status)
			}
		}
	}
}

func TestPingCloseDuringWaitDoesNotRequeue(t *testing.T) {
	var sessions *Sessions
	h := newHarnessWith(t, nil, func(_ *harness, s *Sessions) { sessions = s })
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "ping", Args: json.RawMessage(`{"host":"8.8.8.8"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run()

	ts, _ := h.st.ListTasks(ctx, serial, 10)
	ping := findTask(t, ts, "ping")
	// A waiting ping is running with phase=requested; a lost result read keeps
	// it there instead of requeueing or marking it unknown.
	s := &cwmp.Session{DeviceID: serial, Data: &state{
		root: "InternetGatewayDevice.",
		cur:  &ping,
	}}
	inflight := &cwmp.Request{Tag: tag{"task", "pingget"}}
	sessions.Close(s, inflight)

	ts, _ = h.st.ListTasks(ctx, serial, 10)
	ping = findTask(t, ts, "ping")
	if ping.Status != "running" {
		t.Fatalf("ping after Close = %s, want running", ping.Status)
	}
}

func TestPingExpiresWhileWaiting(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "ping", Args: json.RawMessage(`{"host":"8.8.8.8"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run()

	ts, _ := h.st.ListTasks(ctx, serial, 10)
	ping := findTask(t, ts, "ping")
	if ping.Status != "running" {
		t.Fatalf("ping = %s, want running", ping.Status)
	}
	if _, err := h.st.Pool().Exec(ctx,
		`UPDATE tasks SET expires_at = now() - interval '1 minute' WHERE id = $1`, ping.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := h.st.ExpireTasks(ctx); err != nil || n != 1 {
		t.Fatalf("ExpireTasks = %d, %v, want 1", n, err)
	}
	ts, _ = h.st.ListTasks(ctx, serial, 10)
	if ping = findTask(t, ts, "ping"); ping.Status != "expired" {
		t.Fatalf("ping = %s, want expired", ping.Status)
	}
}

func TestPingArgsValidationAndDefaults(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	bad := []struct {
		args string
		want string
	}{
		{`{}`, "host is required"},
		{`{"host":"8.8.8.8 9"}`, "whitespace"},
		{`{"host":"8.8.8.8","count":-1}`, "count"},
		{`{"host":"8.8.8.8","count":101}`, "count"},
		{`{"host":"8.8.8.8","timeout_ms":-1}`, "timeout_ms"},
		{`{"host":"8.8.8.8","timeout_ms":60001}`, "timeout_ms"},
		{`{"host":"8.8.8.8","size":4}`, "size"},
		{`{"host":"8.8.8.8","size":70000}`, "size"},
	}
	for _, tc := range bad {
		_, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{Name: "ping", Args: json.RawMessage(tc.args)}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %s: err = %v, want %q", tc.args, err, tc.want)
		}
	}

	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "ping", Args: json.RawMessage(`{"host":"8.8.8.8"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	ts, _ := h.st.ListTasks(ctx, serial, 10)
	ping := findTask(t, ts, "ping")
	var a PingArgs
	if err := json.Unmarshal(ping.Args, &a); err != nil {
		t.Fatal(err)
	}
	if a.Count != 4 || a.TimeoutMS != 1000 || a.Size != 64 {
		t.Errorf("defaults = %+v, want count 4 timeout 1000 size 64", a)
	}
}
