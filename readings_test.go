package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadAmbient(t *testing.T) {
	good := `{"accessories":[{"aid":91,"services":[{"type":"3E","characteristics":[{"type":"23","value":"cupboard"}]},{"type":"8A","characteristics":[{"type":"11","value":23.4}]}]}]}`
	body := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/accessories" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	got, err := ReadAmbient(context.Background(), srv.Client(), srv.URL, "cupboard")
	if err != nil || got != 23.4 {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"accessories":[{"services":[{"type":"3E","characteristics":[{"type":"23","value":"cupboard"}]},{"type":"8A","characteristics":[{"type":"11","value":null}]}]}]}`, `garbage`} {
		body = bad
		if _, err := ReadAmbient(context.Background(), srv.Client(), srv.URL, "cupboard"); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	body = good
	if _, err := ReadAmbient(context.Background(), srv.Client(), srv.URL, "other"); err == nil {
		t.Fatal("accepted missing sensor")
	}
	body = strings.NewReplacer(`"3E"`, `"0000003E-0000-1000-8000-0026BB765291"`, `"11"`, `"00000011-0000-1000-8000-0026BB765291"`).Replace(good)
	if got, err := ReadAmbient(context.Background(), srv.Client(), srv.URL, "cupboard"); err != nil || got != 23.4 {
		t.Fatal("full UUID discovery failed", got, err)
	}
	accessory := strings.TrimSuffix(strings.TrimPrefix(good, `{"accessories":[`), `]}`)
	body = `{"accessories":[` + accessory + `,` + accessory + `]}`
	if _, err := ReadAmbient(context.Background(), srv.Client(), srv.URL, "cupboard"); err == nil {
		t.Fatal("accepted ambiguous sensor")
	}
}

func TestParseDisks(t *testing.T) {
	good := []byte(`{"disks":[{"device":"/dev/sda","temperature":33},{"device":"/dev/sdb","temperature":51},{"device":"/dev/sdc","standby":true}]}`)
	got, err := ParseDisks(good, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"})
	if err != nil || len(got) != 3 || *got[1].Temperature != 51 || !got[2].Standby {
		t.Fatalf("%+v %v", got, err)
	}
	// Partial results must preserve the hot disk even if another disk is missing.
	got, err = ParseDisks(good, []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"})
	if err == nil || len(got) != 3 || *got[1].Temperature != 51 {
		t.Fatalf("lost partial hot reading: %+v %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"disks":[{"device":"/dev/sda","temperature":99}]}`, `{"disks":[{"device":"/dev/sda","temperature":33},{"device":"/dev/sda","temperature":34}]}`, `{"disks":[{"device":"/dev/sda","error":"permission denied"}]}`} {
		if _, err := ParseDisks([]byte(bad), []string{"/dev/sda"}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
