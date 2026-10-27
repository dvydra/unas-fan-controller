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
	"slices"
	"strconv"
	"time"
)

func (c *Controller) experimentDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	derived := map[string]func([]byte) ([]byte, error){"samples.csv": experimentCSV, "blocks.csv": experimentBlocksCSV, "pairs.csv": experimentPairsCSV}
	if name != "samples.jsonl" && name != "events.jsonl" && derived[name] == nil {
		http.NotFound(w, r)
		return
	}
	source := name
	if derived[name] != nil {
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
	if convert := derived[name]; convert != nil {
		data, err = convert(data)
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

type blockSummary struct {
	block, pwm              int
	start, last             time.Time
	settled                 int
	hottest, cupboard, room float64
	roomSamples             int
}

func (b blockSummary) complete() bool { return b.last.Sub(b.start) >= experimentBlock-2*time.Minute }
func (b blockSummary) rise() float64  { return b.hottest - b.cupboard }

// summariseBlocks averages the settled second half of each block's latest hold.
// Cupboard temperature is subtracted per block so speeds compare at equivalent ambient.
func summariseBlocks(data []byte) ([]blockSummary, error) {
	type key struct {
		block int
		start time.Time
	}
	attempts := map[key]*blockSummary{}
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var s ExperimentSample
		if err := json.Unmarshal(scan.Bytes(), &s); err != nil {
			return nil, err
		}
		e := s.Controller.Experiment
		if e == nil || e.Phase != "running" || e.BlockStartedAt.IsZero() || s.Controller.Ambient == nil || s.Time.Before(e.BlockStartedAt) {
			continue
		}
		hottest, ok := 0.0, false
		for _, d := range s.Controller.Disks {
			if !d.Standby && d.Temperature != nil && d.Error == "" {
				hottest, ok = max(hottest, *d.Temperature), true
			}
		}
		if !ok {
			continue
		}
		k := key{e.Block, e.BlockStartedAt.UTC()}
		b := attempts[k]
		if b == nil {
			b = &blockSummary{block: e.Block, pwm: e.TargetPWM, start: k.start}
			attempts[k] = b
		}
		b.last = s.Time
		if s.Time.Sub(b.start) < experimentBlock/2 {
			continue
		}
		b.settled++
		b.hottest += hottest
		b.cupboard += *s.Controller.Ambient
		if s.Telemetry.Room != nil {
			b.roomSamples++
			b.room += *s.Telemetry.Room
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	latest := map[int]blockSummary{}
	for _, b := range attempts {
		if b.settled > 0 {
			b.hottest /= float64(b.settled)
			b.cupboard /= float64(b.settled)
		}
		if b.roomSamples > 0 {
			b.room /= float64(b.roomSamples)
		}
		if old, ok := latest[b.block]; !ok || b.start.After(old.start) {
			latest[b.block] = *b
		}
	}
	out := make([]blockSummary, 0, len(latest))
	for _, b := range latest {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b blockSummary) int { return a.block - b.block })
	return out, nil
}

func celsius(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func experimentBlocksCSV(data []byte) ([]byte, error) {
	blocks, err := summariseBlocks(data)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	writer.Write([]string{"block", "pair", "target_pwm", "started_at", "complete", "settled_samples", "hottest_c", "cupboard_c", "room_c", "rise_c"})
	for _, b := range blocks {
		room := ""
		if b.roomSamples > 0 {
			room = celsius(b.room)
		}
		writer.Write([]string{strconv.Itoa(b.block + 1), strconv.Itoa(b.block/2 + 1), strconv.Itoa(b.pwm), b.start.Format(time.RFC3339), strconv.FormatBool(b.complete()), strconv.Itoa(b.settled), celsius(b.hottest), celsius(b.cupboard), room, celsius(b.rise())})
	}
	writer.Flush()
	return out.Bytes(), writer.Error()
}

// experimentPairsCSV compares the two speeds within each adjacent pair of complete blocks,
// which ran hours apart at similar cupboard temperatures.
func experimentPairsCSV(data []byte) ([]byte, error) {
	blocks, err := summariseBlocks(data)
	if err != nil {
		return nil, err
	}
	pairs := map[int]map[int]blockSummary{}
	for _, b := range blocks {
		if !b.complete() {
			continue
		}
		if pairs[b.block/2] == nil {
			pairs[b.block/2] = map[int]blockSummary{}
		}
		pairs[b.block/2][b.pwm] = b
	}
	low, high := experimentLevels[0], experimentLevels[1]
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	writer.Write([]string{"pair", fmt.Sprintf("rise_%d_c", low), fmt.Sprintf("rise_%d_c", high), "rise_difference_c", fmt.Sprintf("cupboard_%d_c", low), fmt.Sprintf("cupboard_%d_c", high), "cupboard_difference_c"})
	for pair := range experimentBlocks / 2 {
		a, okA := pairs[pair][low]
		b, okB := pairs[pair][high]
		if !okA || !okB {
			continue
		}
		writer.Write([]string{strconv.Itoa(pair + 1), celsius(a.rise()), celsius(b.rise()), celsius(a.rise() - b.rise()), celsius(a.cupboard), celsius(b.cupboard), celsius(a.cupboard - b.cupboard)})
	}
	writer.Flush()
	return out.Bytes(), writer.Error()
}
