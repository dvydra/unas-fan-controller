package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeHardware struct {
	pwm           [2]int
	disk, ambient float64
	missing       bool
	partial       bool
	writes        int
}

func (f *fakeHardware) io() ControllerIO {
	return ControllerIO{
		Ambient: func(context.Context) (float64, error) {
			if f.missing {
				return 0, errors.New("offline")
			}
			return f.ambient, nil
		},
		Disks: func(context.Context) ([]DiskReading, error) {
			v := f.disk
			return []DiskReading{{Device: "/dev/sda", Temperature: &v}}, nil
		},
		ReadPWM: func(context.Context) ([2]int, error) { return f.pwm, nil },
		WritePWM: func(_ context.Context, p int) error {
			f.writes++
			f.pwm[0] = p
			if f.partial {
				return errors.New("second fan failed")
			}
			f.pwm[1] = p
			return nil
		},
	}
}
func testController(t *testing.T, f *fakeHardware, now time.Time, dry bool) (*Controller, ControllerConfig) {
	t.Helper()
	cfg := ControllerConfig{HomebridgeURL: "http://example", Accessory: "cupboard", Disks: []string{"/dev/sda"}, StatePath: filepath.Join(t.TempDir(), "state.json"), Mode: "auto", DryRun: dry}
	c, err := NewController(cfg, f.io(), now)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg
}

func TestControllerHoldRestartAndManual(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 41, ambient: 23}
	c, cfg := testController(t, f, now, false)
	c.Sample(ctx, now.Add(holdPeriod-time.Nanosecond))
	if f.pwm[0] != 80 {
		t.Fatal("changed before hold")
	}
	c.Sample(ctx, now.Add(holdPeriod))
	if f.pwm != [2]int{96, 96} {
		t.Fatalf("%v", f.pwm)
	}
	c, err := NewController(cfg, f.io(), now.Add(holdPeriod+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	c.Sample(ctx, now.Add(2*holdPeriod-time.Second))
	if f.pwm[0] != 96 {
		t.Fatal("restart lost hold")
	}
	if err = c.SetManual(ctx, 65, now.Add(2*holdPeriod)); err != nil {
		t.Fatal(err)
	}
	f.disk = 55
	c.Sample(ctx, now.Add(3*holdPeriod))
	if f.pwm[0] != 65 || c.Status().Mode != "manual" {
		t.Fatal("manual overwritten")
	}
	c, err = NewController(cfg, f.io(), now.Add(4*holdPeriod))
	if err != nil || c.Status().Mode != "manual" {
		t.Fatal("manual not persisted", err)
	}
}

func TestControllerFallbackAndRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{38, 38}, disk: 33, ambient: 20}
	c, _ := testController(t, f, now, false)
	c.Sample(ctx, now)
	f.missing = true
	c.Sample(ctx, now.Add(89*time.Second))
	if f.pwm[0] != 38 {
		t.Fatal("transient error escalated")
	}
	c.Sample(ctx, now.Add(90*time.Second))
	if f.pwm != [2]int{167, 167} {
		t.Fatal("no fallback", f.pwm)
	}
	f.missing = false
	c.Sample(ctx, now.Add(91*time.Second))
	if f.pwm[0] != 167 {
		t.Fatal("reduced too soon")
	}
	c.Sample(ctx, now.Add(91*time.Second+holdPeriod))
	if f.pwm[0] != 159 {
		t.Fatal("did not recover gradually", f.pwm)
	}
	f.missing = true
	f.disk = 50
	c.Sample(ctx, now.Add(91*time.Second+holdPeriod))
	if f.pwm[0] != 255 {
		t.Fatal("hot disk failed to override outage")
	}
}

func TestControllerPartialWriteAndDryRun(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	for _, dry := range []bool{false, true} {
		f := &fakeHardware{pwm: [2]int{38, 38}, disk: 50, ambient: 20, partial: true}
		c, _ := testController(t, f, now, dry)
		c.Sample(ctx, now)
		if dry {
			if f.writes != 0 || c.Status().DesiredPWM != 255 {
				t.Fatal("dry-run wrote or lost decision")
			}
			if c.SetManual(ctx, 80, now) == nil {
				t.Fatal("dry-run allowed manual write")
			}
		} else {
			if c.Status().WriteError == "" || !c.Status().LastChange.IsZero() {
				t.Fatal("claimed partial write succeeded")
			}
			f.partial = false
			c.Sample(ctx, now.Add(30*time.Second))
			if f.pwm != [2]int{255, 255} {
				t.Fatal("did not retry safety", f.pwm)
			}
		}
	}
}

func TestHoldStartsAfterSlowWrite(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{38, 38}, disk: 50, ambient: 20}
	c, _ := testController(t, f, now, false)
	write := c.io.WritePWM
	c.io.WritePWM = func(ctx context.Context, pwm int) error {
		time.Sleep(20 * time.Millisecond)
		return write(ctx, pwm)
	}
	c.Sample(context.Background(), now)
	if c.Status().NextOrdinaryAt.Before(now.Add(holdPeriod + 20*time.Millisecond)) {
		t.Fatal("hold started before hardware write completed")
	}
	deadline := c.Status().NextOrdinaryAt
	if err := c.SetMode(context.Background(), "manual", now); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMode(context.Background(), "auto", now); err != nil {
		t.Fatal(err)
	}
	if c.Status().NextOrdinaryAt.Before(deadline) {
		t.Fatal("queued mode changes shortened the previous write's hold")
	}
}

func TestPartialInventoryAndStandby(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{38, 38}, disk: 50, ambient: 20}
	c, _ := testController(t, f, now, false)
	c.cfg.Disks = append(c.cfg.Disks, "/dev/sdb")
	c.Sample(context.Background(), now)
	if f.pwm != [2]int{255, 255} {
		t.Fatal("missing disk concealed a hot disk")
	}
	f.pwm = [2]int{80, 80}
	c.io.Disks = func(context.Context) ([]DiskReading, error) {
		return []DiskReading{{Device: "/dev/sda", Standby: true}, {Device: "/dev/sdb", Standby: true}}, nil
	}
	c.Sample(context.Background(), now.Add(time.Second))
	if f.pwm != [2]int{167, 167} || !c.Status().Disks[0].Standby {
		t.Fatal("all sleeping disks must use fallback, not cached hot data")
	}
}

func TestStateFailureAndCorruptRestart(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 30, ambient: 20}
	c, cfg := testController(t, f, now, false)
	if err := os.WriteFile(cfg.StatePath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewController(cfg, f.io(), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	c.Sample(context.Background(), now.Add(time.Hour+holdPeriod-time.Second))
	if f.writes != 0 {
		t.Fatal("corrupt state bypassed restart hold")
	}
	c.cfg.StatePath = filepath.Join(t.TempDir(), "missing", "state.json")
	c.Sample(context.Background(), now.Add(2*time.Hour))
	if f.writes != 0 || c.Status().StateError == "" {
		t.Fatal("reduced without persistent hold")
	}
	f.disk = 50
	c.Sample(context.Background(), now.Add(2*time.Hour+time.Second))
	if f.pwm != [2]int{255, 255} {
		t.Fatal("state failure blocked emergency cooling")
	}
}

func TestConcurrentManualWinsOverRunningSample(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeHardware{pwm: [2]int{80, 80}, disk: 41, ambient: 20}
	c, _ := testController(t, f, now, false)
	entered, release, sampled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	disks := c.io.Disks
	c.io.Disks = func(ctx context.Context) ([]DiskReading, error) {
		close(entered)
		<-release
		return disks(ctx)
	}
	go func() { c.Sample(context.Background(), now.Add(holdPeriod)); close(sampled) }()
	<-entered
	manual := make(chan error, 1)
	go func() { manual <- c.SetManual(context.Background(), 65, now.Add(holdPeriod)) }()
	close(release)
	<-sampled
	if err := <-manual; err != nil {
		t.Fatal(err)
	}
	if f.pwm != [2]int{65, 65} || c.Status().Mode != "manual" {
		t.Fatal("automatic sample overwrote manual action")
	}
}
