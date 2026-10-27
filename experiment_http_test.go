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

func TestExperimentBlockAnalysis(t *testing.T) {
	start := time.Unix(100000, 0).UTC()
	var lines []string
	add := func(block, pwm int, blockStart time.Time, minutes int, cupboard, disk float64) {
		for m := 0; m < minutes; m++ {
			at := blockStart.Add(time.Duration(m) * time.Minute)
			// Unsettled first hour reads hot; only the second hour may count.
			d := disk
			if m < 60 {
				d += 5
			}
			room := cupboard - 5
			s := ExperimentSample{Time: at, Telemetry: Telemetry{Room: &room}, Controller: ControllerStatus{
				Ambient: &cupboard, PWM: [2]int{pwm, pwm},
				Disks:      []DiskReading{{Device: "/dev/sda", Temperature: &d}, {Device: "/dev/sdb", Temperature: ptr(d - 2)}},
				Experiment: &ExperimentStatus{Phase: "running", Block: block, TargetPWM: pwm, BlockStartedAt: blockStart},
			}}
			b, _ := json.Marshal(s)
			lines = append(lines, string(b))
		}
	}
	add(0, 38, start, 30, 30, 50)                   // interrupted attempt, later retried
	add(0, 38, start.Add(time.Hour), 120, 24, 44)   // rise 20
	add(1, 56, start.Add(3*time.Hour), 120, 25, 38) // rise 13
	add(2, 56, start.Add(5*time.Hour), 90, 22, 35)  // incomplete
	data := []byte(strings.Join(lines, "\n") + "\n")

	read := func(b []byte, err error) []map[string]string {
		t.Helper()
		rows, err2 := csv.NewReader(strings.NewReader(string(b))).ReadAll()
		if err != nil || err2 != nil {
			t.Fatal(err, err2)
		}
		var out []map[string]string
		for _, r := range rows[1:] {
			m := map[string]string{}
			for i, k := range rows[0] {
				m[k] = r[i]
			}
			out = append(out, m)
		}
		return out
	}
	blocks := read(experimentBlocksCSV(data))
	if len(blocks) != 3 {
		t.Fatal(blocks)
	}
	b0 := blocks[0]
	if b0["block"] != "1" || b0["target_pwm"] != "38" || b0["complete"] != "true" || b0["settled_samples"] != "60" || b0["hottest_c"] != "44.00" || b0["cupboard_c"] != "24.00" || b0["room_c"] != "19.00" || b0["rise_c"] != "20.00" {
		t.Fatal("retried block summarised wrongly", b0)
	}
	if blocks[2]["complete"] != "false" {
		t.Fatal("incomplete block marked complete", blocks[2])
	}
	pairs := read(experimentPairsCSV(data))
	if len(pairs) != 1 {
		t.Fatal("only complete pairs count", pairs)
	}
	p := pairs[0]
	if p["pair"] != "1" || p["rise_38_c"] != "20.00" || p["rise_56_c"] != "13.00" || p["rise_difference_c"] != "7.00" || p["cupboard_difference_c"] != "-1.00" {
		t.Fatal(p)
	}

	now := time.Unix(100000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
	c, _, _ := experimentalController(t, f, now)
	c.Sample(context.Background(), now)
	h := newMux(c, func(context.Context) (string, error) { return "", nil })
	for _, file := range []string{"blocks.csv", "pairs.csv"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/experiment/"+file, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
			t.Fatal(file, w.Code, w.Body.String())
		}
	}
}

func ptr(v float64) *float64 { return &v }
