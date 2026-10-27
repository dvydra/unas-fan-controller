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
	if s.Experiment.Phase != "running" || f.pwm != [2]int{38, 38} {
		t.Fatalf("did not start first block: %+v %v", s.Experiment, f.pwm)
	}
	c, err := NewController(cfg, io, now.Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Status().Experiment; e.Phase != "running" || e.Block != 0 || !e.BlockStartedAt.IsZero() {
		t.Fatal("restart must restart the interrupted block, not abort or resume it", e)
	}
	c.Sample(context.Background(), now.Add(31*time.Minute+30*time.Second))
	if e := c.Status().Experiment; e.Phase != "running" || e.Block != 0 || e.BlockStartedAt.IsZero() || f.pwm != [2]int{38, 38} {
		t.Fatal("interrupted block did not restart", e, f.pwm)
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
	slots := [12]map[int]int{}
	pairs := map[int]map[int]bool{}
	var lastBlock time.Time
	// Run the real controller across the full experiment with a controlled clock.
	for i := 0; i <= int((experimentBlocks*2*time.Hour+time.Hour)/(30*time.Second)); i++ {
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
				slots[slot] = map[int]int{}
			}
			slots[slot][f.pwm[0]]++
			if pairs[previous/2] == nil {
				pairs[previous/2] = map[int]bool{}
			}
			pairs[previous/2][f.pwm[0]] = true
		}
	}
	if c.Status().Experiment.Phase != "completed" || blocks != 48 || counts[38] != 24 || counts[56] != 24 {
		t.Fatal(c.Status().Experiment, counts, blocks)
	}
	for _, speeds := range slots {
		if speeds[38] != 2 || speeds[56] != 2 {
			t.Fatal("unbalanced time-of-day slot", speeds)
		}
	}
	// Adjacent blocks are the ambient-matched comparison, so every pair must contain both speeds.
	for pair, speeds := range pairs {
		if len(speeds) != 2 {
			t.Fatal("pair without both speeds", pair, speeds)
		}
	}
}

func TestExperimentAbortAndManualCancellation(t *testing.T) {
	for _, scenario := range []string{"hot", "emergency", "write", "manual"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Unix(100000, 0)
			f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
			c, cfg, io := experimentalController(t, f, now)
			for i := 0; i <= 60; i++ {
				c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
			}
			switch scenario {
			case "hot":
				f.disk = 49
			case "emergency":
				f.disk = 50
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
			} else {
				c.Sample(context.Background(), now.Add(31*time.Minute))
			}
			s := c.Status()
			if scenario == "write" && s.WriteError == "" {
				t.Fatal("test did not exercise a failed write")
			}
			if scenario == "hot" && f.pwm[0] < 167 {
				t.Fatal("49°C abort failed to raise cooling")
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

func TestExperimentToleratesReadingLoss(t *testing.T) {
	start := func(t *testing.T) (*Controller, *fakeHardware, time.Time) {
		now := time.Unix(100000, 0)
		f := &fakeHardware{pwm: [2]int{80, 80}, disk: 39, ambient: 22}
		c, _, _ := experimentalController(t, f, now)
		for i := 0; i <= 80; i++ {
			c.Sample(context.Background(), now.Add(time.Duration(i)*30*time.Second))
		}
		if e := c.Status().Experiment; e.Phase != "running" || e.BlockStartedAt.IsZero() {
			t.Fatal("block not holding", e)
		}
		return c, f, now.Add(40 * time.Minute)
	}
	t.Run("transient", func(t *testing.T) {
		c, f, now := start(t)
		held := c.Status().Experiment.BlockStartedAt
		// One failed cupboard read, as on 2026-10-26, must not cost the block.
		f.missing = true
		c.Sample(context.Background(), now.Add(30*time.Second))
		f.missing = false
		c.io.Telemetry = func(context.Context) (Telemetry, error) { return Telemetry{}, errors.New("room unavailable") }
		c.Sample(context.Background(), now.Add(time.Minute))
		if e := c.Status().Experiment; e.Phase != "running" || !e.BlockStartedAt.Equal(held) || f.pwm != [2]int{38, 38} {
			t.Fatal("transient loss disturbed the block", e, f.pwm)
		}
	})
	for _, scenario := range []string{"missing", "gap"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, now := start(t)
			block := c.Status().Experiment.Block
			next := now.Add(30 * time.Second)
			if scenario == "missing" {
				f.missing = true
				for i := 1; i <= 4; i++ {
					next = now.Add(time.Duration(i) * 30 * time.Second)
					c.Sample(context.Background(), next)
				}
				if f.pwm[0] < 167 {
					t.Fatal("lost readings did not fall back to safe cooling", f.pwm)
				}
				f.missing = false
			} else {
				next = now.Add(5 * time.Minute)
				c.Sample(context.Background(), next)
			}
			if e := c.Status().Experiment; e.Phase != "running" || e.Block != block || !e.BlockStartedAt.IsZero() {
				t.Fatal("lost readings must restart the block, not abort or keep its data", e)
			}
			for i := 1; i <= 25; i++ {
				c.Sample(context.Background(), next.Add(time.Duration(i)*30*time.Second))
			}
			if e := c.Status().Experiment; e.Phase != "running" || e.Block != block || e.BlockStartedAt.IsZero() || f.pwm != [2]int{38, 38} {
				t.Fatal("block did not restart after readings returned", e, f.pwm)
			}
		})
	}
}
