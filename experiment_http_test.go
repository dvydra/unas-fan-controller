package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExperimentDownloads(t *testing.T) {
	now := time.Unix(100000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
	c, _, _ := experimentalController(t, f, now)
	c.Sample(context.Background(), now)
	h := newMux(c, func(context.Context) (string, error) { return "", nil })
	for _, file := range []string{"samples.jsonl", "samples.csv", "events.jsonl"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/experiment/"+file, nil))
		if w.Code != 200 {
			t.Fatal(file, w.Code, w.Body.String())
		}
		if file == "samples.jsonl" {
			var s ExperimentSample
			if json.Unmarshal(w.Body.Bytes(), &s) != nil || *s.Telemetry.Room != 21.5 || *s.Controller.Disks[0].Temperature != 39 {
				t.Fatal(w.Body.String())
			}
		}
		if file == "samples.csv" {
			rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			values := map[string]string{}
			for i, k := range rows[0] {
				values[k] = rows[1][i]
			}
			if values["room_c"] != "21.5" || values["disk_c"] != "39" || values["read_sectors"] != "200" {
				t.Fatal(values)
			}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/experiment/state.json", nil))
	if w.Code != 404 {
		t.Fatal("unexpected file exposed")
	}
	// Polling jitter must not turn minute capture into alternating 90-second gaps.
	c.Sample(context.Background(), now.Add(time.Minute-time.Millisecond))
	c.Sample(context.Background(), now.Add(2*time.Minute-2*time.Millisecond))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/experiment/samples.csv", nil))
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(rows) != 4 {
		t.Fatal("minute samples missing", rows, err)
	}
}
