package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHookHandlerAuthAndReply(t *testing.T) {
	var got Step
	h := HookHandler("s3cret", func(_ context.Context, s Step) ([]Task, error) {
		got = s
		return []Task{Set(Value{Path: "A", Value: "b"})}, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body, _ := json.Marshal(Step{
		Step: 1, Device: Device{ID: "d", Serial: "s"},
		Events: []string{"0 BOOTSTRAP"}, Params: map[string]Param{"a": {Value: "v"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var reply Reply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Tasks) != 1 || reply.Tasks[0].Name != "set" {
		t.Fatalf("reply = %+v", reply)
	}
	if got.Device.Serial != "s" || got.Params["a"].Value != "v" || got.Events[0] != "0 BOOTSTRAP" {
		t.Errorf("handler saw %+v", got)
	}

	unauthorized, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	unauthorized.Header.Set("Authorization", "Bearer wrong")
	resp, err = srv.Client().Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong secret = %d, want 401", resp.StatusCode)
	}

	get, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	get.Header.Set("Authorization", "Bearer s3cret")
	resp, err = srv.Client().Do(get)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", resp.StatusCode)
	}

	bad, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("not json"))
	bad.Header.Set("Authorization", "Bearer s3cret")
	resp, err = srv.Client().Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", resp.StatusCode)
	}
}

func TestHookHandlerFuncState(t *testing.T) {
	h := HandlerFunc("k", func(_ context.Context, s Step) (Reply, error) {
		return Reply{Tasks: []Task{Reboot()}, State: "next:" + s.State}, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body, _ := json.Marshal(Step{Step: 2, State: "x"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var reply Reply
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if reply.State != "next:x" || len(reply.Tasks) != 1 {
		t.Errorf("reply = %+v", reply)
	}
}

func TestHookStepWireShape(t *testing.T) {
	// The provisioner payload must not grow fields that only the API reads.
	b, err := json.Marshal(Step{Step: 1, Device: Device{ID: "d", Serial: "s"}, Events: []string{"0 BOOTSTRAP"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"step":1,"device":{"id":"d","serial":"s","oui":"","product_class":"","software_version":""},"events":["0 BOOTSTRAP"]}`
	if string(b) != want {
		t.Errorf("Step = %s, want %s", b, want)
	}
}

func TestMatches(t *testing.T) {
	for _, tc := range []struct {
		events, triggers []string
		want             bool
	}{
		{[]string{"0 BOOTSTRAP"}, []string{"0 BOOTSTRAP"}, true},
		{[]string{"0 BOOTSTRAP", "1 BOOT"}, []string{"0"}, true},
		{[]string{"2 PERIODIC"}, []string{"0 BOOTSTRAP"}, false},
		{[]string{"2 PERIODIC"}, nil, false},
	} {
		if got := Matches(tc.events, tc.triggers); got != tc.want {
			t.Errorf("Matches(%v,%v) = %v", tc.events, tc.triggers, got)
		}
	}
}
