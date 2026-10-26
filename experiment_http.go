package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func (c *Controller) experimentDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name != "samples.jsonl" && name != "samples.csv" && name != "events.jsonl" {
		http.NotFound(w, r)
		return
	}
	source := name
	if name == "samples.csv" {
		source = "samples.jsonl"
	}
	// Snapshot under the sampling lock, then release it before serving a slow client.
	c.op.Lock()
	if c.experiment == nil || c.experiment.dir == "" {
		c.op.Unlock()
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(c.experiment.dir, source))
	c.op.Unlock()
	if err != nil {
		http.Error(w, "Capture unavailable", 503)
		return
	}
	if name == "samples.csv" {
		data, err = experimentCSV(data)
		if err != nil {
			http.Error(w, "Capture contains invalid JSON", 500)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func experimentCSV(data []byte) ([]byte, error) {
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	writer.Write([]string{"time", "phase", "block", "block_started_at", "target_pwm", "pwm1", "pwm2", "rpm1", "rpm2", "room_c", "cupboard_c", "device", "disk_c", "standby", "read_sectors", "write_sectors", "io_millis", "nas_uptime_seconds", "read_error", "write_error", "telemetry_error"})
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), 1<<20)
	number := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	for scan.Scan() {
		var s ExperimentSample
		if err := json.Unmarshal(scan.Bytes(), &s); err != nil {
			return nil, err
		}
		e := s.Controller.Experiment
		if e == nil {
			continue
		}
		activities := map[string]DiskActivity{}
		for _, a := range s.Telemetry.DiskActivity {
			activities[a.Device] = a
		}
		for _, d := range s.Controller.Disks {
			reads, writes, busy := "", "", ""
			if a, ok := activities[d.Device]; ok {
				reads = strconv.FormatUint(a.ReadSectors, 10)
				writes = strconv.FormatUint(a.WriteSectors, 10)
				busy = strconv.FormatUint(a.IOMillis, 10)
			}
			writer.Write([]string{s.Time.UTC().Format(time.RFC3339Nano), e.Phase, strconv.Itoa(e.Block), e.BlockStartedAt.UTC().Format(time.RFC3339Nano), strconv.Itoa(e.TargetPWM), strconv.Itoa(s.Controller.PWM[0]), strconv.Itoa(s.Controller.PWM[1]), strconv.Itoa(s.Telemetry.RPM[0]), strconv.Itoa(s.Telemetry.RPM[1]), number(s.Telemetry.Room), number(s.Controller.Ambient), d.Device, number(d.Temperature), strconv.FormatBool(d.Standby), reads, writes, busy, strconv.FormatFloat(s.Telemetry.UptimeSeconds, 'f', -1, 64), s.Controller.ReadError, s.Controller.WriteError, s.TelemetryError})
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	writer.Flush()
	return out.Bytes(), writer.Error()
}
