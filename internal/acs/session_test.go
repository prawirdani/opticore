package acs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/prawirdani/opticore/internal/config"
	"github.com/prawirdani/opticore/internal/cwmp"
	"github.com/prawirdani/opticore/internal/store"
)

var (
	testDSN string
	testKey = []byte("0123456789abcdef0123456789abcdef")
)

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
	if _, err := s.Pool().Exec(ctx, `TRUNCATE tasks, events, devices RESTART IDENTITY CASCADE`); err != nil {
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

func newHarness(t *testing.T, configure func(*fakeCPE)) *harness {
	t.Helper()
	st := testStore(t)
	cs := &cwmp.Server{
		Handler: &Sessions{Store: st, Key: testKey, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), GPVBatch: 32, MaxAttempts: 2},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	return &harness{t: t, acs: srv, svc: &Service{Store: st, Key: testKey}, st: st, cpe: f.client, sess: f}
}

// fakeCPE answers ACS requests from canned tables and records what it saw.
type fakeCPE struct {
	t      *testing.T
	url    string
	client *http.Client

	params   map[string]string // GPV answers
	faultSPV *string           // fault code to return on SPV instead of a response
	faultGPV *string

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
	f.t.Helper()
	body := fmt.Sprintf(informTpl, serial, eventList(events), time.Now().UTC().Format(time.RFC3339))
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
<cwmp:DeviceID><cwmp:Manufacturer>Huawei</cwmp:Manufacturer><cwmp:OUI>00259E</cwmp:OUI><cwmp:ProductClass>HG8145V5</cwmp:ProductClass><cwmp:SerialNumber>%s</cwmp:SerialNumber></cwmp:DeviceID>
<cwmp:EventList soap-enc:arrayType="cwmp:EventStruct[1]">%s</cwmp:EventList>
<cwmp:MaxEnvelopes>1</cwmp:MaxEnvelopes>
<cwmp:CurrentTime>%s</cwmp:CurrentTime>
<cwmp:RetryCount>0</cwmp:RetryCount>
<cwmp:ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[4]">
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.ConnectionRequestURL</cwmp:Name><cwmp:Value>http://10.20.3.41:7547/abc</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.PeriodicInformInterval</cwmp:Name><cwmp:Value>300</cwmp:Value><cwmp:Type xsi:type="xsd:unsignedInt">xsd:unsignedInt</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.DeviceInfo.SoftwareVersion</cwmp:Name><cwmp:Value>V5R020C10S271</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
<cwmp:ParameterValueStruct><cwmp:Name>InternetGatewayDevice.ManagementServer.ConnectionRequestPassword</cwmp:Name><cwmp:Value>leaked</cwmp:Value><cwmp:Type xsi:type="xsd:string">xsd:string</cwmp:Type></cwmp:ParameterValueStruct>
</cwmp:ParameterList>
</cwmp:Inform></soap-env:Body></soap-env:Envelope>`

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
	if p := d.Params["InternetGatewayDevice.ManagementServer.ConnectionRequestPassword"]; !p.Secret || p.Value != "" {
		t.Errorf("inform password cached as %+v, want secret marker only", p)
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

func TestSecretSetIsSealedAtRestAndDecryptedOnTheWire(t *testing.T) {
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
	if strings.Contains(string(ts[0].Args), "hunter2") {
		t.Errorf("args stored plaintext: %s", ts[0].Args)
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
	code, body := h.sess.post(fmt.Sprintf(informTpl, "48575443BD7589AB", eventList([]string{"1 BOOT"}), time.Now().UTC().Format(time.RFC3339)))
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
