package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func experimentalController(t *testing.T, f *fakeHardware, now time.Time) (*Controller, ControllerConfig, ControllerIO) {
	t.Helper()
	cfg := ControllerConfig{HomebridgeURL: "http://example", Accessory: "cupboard", Disks: []string{"/dev/sda"}, StatePath: filepath.Join(t.TempDir(), "controller.json"), Mode: "auto", Experiment: &ExperimentConfig{ID: "test", RoomURL: "http://room", RoomAccessory: "living"}}
	io := f.io()
	io.Telemetry = func(context.Context) (Telemetry, error) {
		room := 21.5
		return Telemetry{Room: &room, RPM: [2]int{1300, 1350}, UptimeSeconds: 100, DiskActivity: []DiskActivity{{Device: "/dev/sda", ReadSectors: 200, WriteSectors: 100, IOMillis: 30}}}, nil
	}
	c, err := NewController(cfg, io, now)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg, io
}

func TestExperimentBaselineScheduleAndRestart(t *testing.T) {
	now := time.Unix(100000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
	c, cfg, io := experimentalController(t, f, now)
	for i := 0; i < 60; i++ {
		c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
	}
	if c.Status().Experiment.Phase != "baseline" {
		t.Fatal("started before thirty minutes")
	}
	c.Sample(context.Background(), now.Add(30*time.Minute))
	s := c.Status()
	if s.Experiment.Phase != "running" || f.pwm != [2]int{64, 64} {
		t.Fatalf("did not start first block: %+v %v", s.Experiment, f.pwm)
	}
	c, err := NewController(cfg, io, now.Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if c.Status().Experiment.Phase != "aborted" {
		t.Fatal("restart resumed an interrupted block")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.StatePath), "experiments", "test", "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("no durable samples")
	}
}

func TestExperimentFullSchedule(t *testing.T) {
	now := time.Unix(100000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
	c, _, _ := experimentalController(t, f, now)
	counts := map[int]int{}
	previous := -1
	blocks := 0
	slots := [12]map[int]bool{}
	var lastBlock time.Time
	// Run the real controller across the full experiment with a controlled clock.
	for i := 0; i <= int((73*time.Hour)/(30*time.Second)); i++ {
		c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
		s := c.Status()
		if s.Experiment.Phase == "running" && s.Experiment.Block != previous {
			previous = s.Experiment.Block
			blocks++
			counts[f.pwm[0]]++
			if !lastBlock.IsZero() && s.Experiment.BlockStartedAt.Sub(lastBlock) < 2*time.Hour {
				t.Fatal("block ended early")
			}
			lastBlock = s.Experiment.BlockStartedAt
			slot := previous % 12
			if slots[slot] == nil {
				slots[slot] = map[int]bool{}
			}
			slots[slot][f.pwm[0]] = true
		}
	}
	if c.Status().Experiment.Phase != "completed" || blocks != 36 || counts[48] != 12 || counts[56] != 12 || counts[64] != 12 {
		t.Fatal(c.Status().Experiment, counts, blocks)
	}
	for _, speeds := range slots {
		if len(speeds) != 3 {
			t.Fatal("unbalanced time-of-day slot", speeds)
		}
	}
}

func TestExperimentAbortAndManualCancellation(t *testing.T) {
	for _, scenario := range []string{"hot", "emergency", "missing", "telemetry", "gap", "write", "manual"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(100000, 0)
			f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
			c, cfg, io := experimentalController(t, f, now)
			for i := 0; i <= 60; i++ {
				c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
			}
			switch scenario {
			case "hot":
				f.disk = 45
			case "emergency":
				f.disk = 50
			case "missing":
				f.missing = true
			case "telemetry":
				c.io.Telemetry = func(context.Context) (Telemetry, error) { return Telemetry{}, errors.New("room unavailable") }
			case "write":
				f.partial = true
			case "manual":
				if err := c.SetManual(context.Background(), 70, now.Add(31*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "write" {
				for i := 61; i <= 302; i++ {
					c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
				}
			} else if scenario == "gap" {
				c.Sample(context.Background(), now.Add(32*time.Minute))
			} else {
				c.Sample(context.Background(), now.Add(31*time.Minute))
			}
			s := c.Status()
			if scenario == "write" && s.WriteError == "" {
				t.Fatal("test did not exercise a failed write")
			}
			if scenario == "hot" && f.pwm[0] < 167 {
				t.Fatal("45°C abort failed to raise cooling")
			}
			if scenario == "emergency" && f.pwm != [2]int{255, 255} {
				t.Fatal("experiment masked emergency cooling", f.pwm)
			}
			if scenario == "manual" {
				if s.Experiment.Phase != "cancelled" || f.pwm != [2]int{70, 70} {
					t.Fatal(s.Experiment, f.pwm)
				}
			} else if s.Experiment.Phase != "aborted" {
				t.Fatal("unsafe experiment continued", s.Experiment)
			}
			restarted, err := NewController(cfg, io, now.Add(42*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if restarted.Status().Experiment.Phase != s.Experiment.Phase {
				t.Fatal("terminal state not persisted")
			}
			var persisted map[string]any
			b, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.StatePath), "experiments", "test", "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(b, &persisted) != nil || persisted["phase"] != s.Experiment.Phase {
				t.Fatal("state missing")
			}
		})
	}
}

func TestExperimentStorageFailureKeepsCooling(t *testing.T) {
	for _, scenario := range []string{"corrupt", "capture"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(100000, 0)
			f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
			c, cfg, io := experimentalController(t, f, now)
			dir := filepath.Join(filepath.Dir(cfg.StatePath), "experiments", "test")
			if scenario == "corrupt" {
				if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				c, err = NewController(cfg, io, now)
				if err != nil {
					t.Fatal("experiment corruption disabled cooling", err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(dir, "samples.jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			c.Sample(context.Background(), now)
			c.Sample(context.Background(), now.Add(30*time.Second))
			if c.Status().Experiment.Phase != "aborted" || c.Status().Experiment.Error == "" || f.pwm[0] < 167 {
				t.Fatal(c.Status(), f.pwm)
			}
			c, err := NewController(cfg, io, now.Add(time.Minute))
			if err != nil || c.Status().Experiment.Phase != "aborted" {
				t.Fatal("rearmed failed experiment", err)
			}
			if scenario == "capture" {
				h := newMux(c, func(context.Context) (string, error) { return "", nil })
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/api/experiment/events.jsonl", nil))
				if w.Code != 200 {
					t.Fatal("lost recorded events after restart", w.Code)
				}
			}
		})
	}
}

func TestExperimentBaselineMustBeContinuous(t *testing.T) {
	now := time.Unix(100000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
	c, _, _ := experimentalController(t, f, now)
	for i := 0; i <= 100; i++ {
		f.missing = i == 40
		c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
	}
	if c.Status().Experiment.Phase != "baseline" {
		t.Fatal("started without thirty continuous valid minutes")
	}
	c.Sample(context.Background(), now.Add(50*time.Minute+30*time.Second))
	if c.Status().Experiment.Phase != "running" {
		t.Fatal(c.Status().Experiment)
	}
}
