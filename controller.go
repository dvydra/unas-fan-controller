package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ControllerConfig struct {
	HomebridgeURL string   `json:"homebridge_url"`
	Accessory     string   `json:"accessory"`
	Disks         []string `json:"disks"`
	StatePath     string   `json:"state_path"`
	Mode          string   `json:"mode"`
	DryRun        bool     `json:"dry_run"`
}
type ControllerIO struct {
	Ambient  func(context.Context) (float64, error)
	Disks    func(context.Context) ([]DiskReading, error)
	ReadPWM  func(context.Context) ([2]int, error)
	WritePWM func(context.Context, int) error
}
type ControllerStatus struct {
	Available      bool          `json:"available"`
	Mode           string        `json:"mode"`
	DryRun         bool          `json:"dry_run"`
	Ambient        *float64      `json:"ambient,omitempty"`
	AmbientAt      time.Time     `json:"ambient_at"`
	Disks          []DiskReading `json:"disks"`
	PWM            [2]int        `json:"pwm"`
	PWMAt          time.Time     `json:"pwm_at"`
	DesiredPWM     int           `json:"desired_pwm"`
	Reason         string        `json:"reason"`
	LastChange     time.Time     `json:"last_change"`
	NextOrdinaryAt time.Time     `json:"next_ordinary_at"`
	ReadError      string        `json:"read_error,omitempty"`
	WriteError     string        `json:"write_error,omitempty"`
	StateError     string        `json:"state_error,omitempty"`
}
type persistedState struct {
	Mode       string    `json:"mode"`
	LastChange time.Time `json:"last_change"`
	HoldUntil  time.Time `json:"hold_until"`
}
type Controller struct {
	// op serializes sampling and manual changes, including I/O. mu only protects snapshots.
	op    sync.Mutex
	mu    sync.RWMutex
	cfg   ControllerConfig
	io    ControllerIO
	state ControllerStatus
	disks map[string]DiskReading
}

func NewController(cfg ControllerConfig, io ControllerIO, now time.Time) (*Controller, error) {
	if cfg.Mode == "" {
		cfg.Mode = "manual"
	}
	if cfg.Mode != "manual" && cfg.Mode != "auto" {
		return nil, errors.New("mode must be manual or auto")
	}
	available := cfg.HomebridgeURL != "" || cfg.Accessory != "" || len(cfg.Disks) > 0 || cfg.StatePath != ""
	if available {
		u, err := url.Parse(cfg.HomebridgeURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.Accessory == "" || len(cfg.Disks) == 0 || cfg.StatePath == "" {
			return nil, errors.New("controller requires Homebridge URL, accessory, disk inventory and state path")
		}
		seen := map[string]bool{}
		for _, d := range cfg.Disks {
			if d == "" || seen[d] {
				return nil, errors.New("invalid disk inventory")
			}
			seen[d] = true
		}
	} else if cfg.Mode == "auto" {
		return nil, errors.New("auto mode requires sensor configuration")
	}
	c := &Controller{cfg: cfg, io: io, disks: map[string]DiskReading{}, state: ControllerStatus{Available: available, Mode: cfg.Mode, DryRun: cfg.DryRun, NextOrdinaryAt: now.Add(holdPeriod), Reason: "Waiting for first sample"}}
	if available {
		data, err := os.ReadFile(cfg.StatePath)
		var p persistedState
		if err == nil && json.Unmarshal(data, &p) == nil && (p.Mode == "auto" || p.Mode == "manual") {
			c.state.Mode = p.Mode
			c.state.LastChange = p.LastChange
			c.state.NextOrdinaryAt = p.HoldUntil
			if p.LastChange.Add(holdPeriod).After(c.state.NextOrdinaryAt) {
				c.state.NextOrdinaryAt = p.LastChange.Add(holdPeriod)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read controller state: %w", err)
		}
		if err = c.persist(&c.state); err != nil {
			return nil, fmt.Errorf("write controller state: %w", err)
		}
	}
	return c, nil
}

func (c *Controller) persist(s *ControllerStatus) error {
	if !s.Available {
		return nil
	}
	data, err := json.Marshal(persistedState{Mode: s.Mode, LastChange: s.LastChange, HoldUntil: s.NextOrdinaryAt})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.cfg.StatePath), ".fan-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.cfg.StatePath)
}
func (c *Controller) Status() ControllerStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.state
	s.Disks = append([]DiskReading(nil), s.Disks...)
	if s.Ambient != nil {
		v := *s.Ambient
		s.Ambient = &v
	}
	for i := range s.Disks {
		if s.Disks[i].Temperature != nil {
			v := *s.Disks[i].Temperature
			s.Disks[i].Temperature = &v
		}
	}
	return s
}
func (c *Controller) publish(s ControllerStatus) { c.mu.Lock(); c.state = s; c.mu.Unlock() }
func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
func fresh(t, now time.Time) bool { return !t.IsZero() && !now.Before(t) && now.Sub(t) < staleAfter }

func (c *Controller) Sample(ctx context.Context, now time.Time) {
	started := time.Now()
	c.op.Lock()
	defer c.op.Unlock()
	s := c.Status()
	defer func() { c.publish(s) }()
	var errs []error
	pwm, err := c.io.ReadPWM(ctx)
	if err != nil {
		s.ReadError = "Read fan speed: " + err.Error()
		s.Reason = "Cannot verify current fan speed"
		return
	}
	if pwm[0] < 0 || pwm[0] > 255 || pwm[1] < 0 || pwm[1] > 255 {
		s.ReadError = "Invalid PWM readback"
		return
	}
	s.PWM = pwm
	s.PWMAt = now
	if !s.Available {
		s.Reason = "Manual control; automatic control not configured"
		s.ReadError = ""
		return
	}
	ambient, ambientErr := c.io.Ambient(ctx)
	if ambientErr == nil && !plausible(ambient, -20, 60) {
		ambientErr = errors.New("invalid ambient reading")
	}
	if ambientErr == nil {
		s.Ambient = &ambient
		s.AmbientAt = now
	} else {
		errs = append(errs, ambientErr)
	}
	disks, diskErr := c.io.Disks(ctx)
	if diskErr != nil {
		errs = append(errs, diskErr)
	}
	reported := map[string]bool{}
	hotNow := false
	hottest := 0.0
	active := 0
	allFresh := true
	for _, d := range disks {
		reported[d.Device] = true
		if d.Error == "" && (d.Standby || (d.Temperature != nil && plausible(*d.Temperature, 0, 80))) {
			d.SampledAt = now
			c.disks[d.Device] = d
			if !d.Standby && d.Temperature != nil && *d.Temperature >= 50 {
				hotNow = true
			}
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid or unavailable reading", d.Device))
		}
	}
	s.Disks = nil
	for _, device := range c.cfg.Disks {
		d, ok := c.disks[device]
		if !ok {
			d = DiskReading{Device: device, Error: "No reading"}
		}
		if !reported[device] {
			errs = append(errs, fmt.Errorf("%s: missing reading", device))
		}
		if !fresh(d.SampledAt, now) {
			allFresh = false
			d.Error = "Reading unavailable or stale"
		}
		if !d.Standby && d.Temperature != nil && fresh(d.SampledAt, now) {
			active++
			hottest = max(hottest, *d.Temperature)
		}
		s.Disks = append(s.Disks, d)
	}
	s.ReadError = errorText(errors.Join(errs...))
	if s.Mode != "auto" {
		s.Reason = "Manual mode; automatic protection disabled"
		s.DesiredPWM = max(pwm[0], pwm[1])
		return
	}
	av := 0.0
	if s.Ambient != nil {
		av = *s.Ambient
	}
	in := PolicyInput{Now: now, NextOrdinaryAt: s.NextOrdinaryAt, Current: max(pwm[0], pwm[1]), Ambient: av, Hottest: hottest, AmbientValid: fresh(s.AmbientAt, now), DiskValid: allFresh && active > 0}
	decision := Decide(in)
	if hotNow {
		decision = Decision{255, "Disk at or above 50°C", true}
	}
	// Never reduce speed based on cached data during a transient failure.
	if (len(errs) > 0 || s.StateError != "") && decision.PWM < in.Current {
		decision = Decision{in.Current, "Holding speed while readings or state recover", false}
	}
	s.DesiredPWM = decision.PWM
	s.Reason = decision.Reason
	if s.StateError != "" {
		if err := c.persist(&s); err == nil {
			s.StateError = ""
		}
	}
	if pwm == [2]int{decision.PWM, decision.PWM} {
		return
	}
	if !decision.Safety && now.Before(s.NextOrdinaryAt) {
		return
	}
	if s.DryRun {
		s.Reason = "Dry run: " + decision.Reason
		return
	}
	c.write(ctx, &s, decision.PWM, now.Add(time.Since(started)))
}

func (c *Controller) write(ctx context.Context, s *ControllerStatus, pwm int, now time.Time) error {
	started := time.Now()
	// Persist the hold BEFORE touching hardware: even a process crash or partial write cannot erase it.
	// Reserve both bounded SSH calls if the process exits before recording completion.
	s.NextOrdinaryAt = now.Add(holdPeriod + 40*time.Second)
	if err := c.persist(s); err != nil {
		s.StateError = err.Error()
		if pwm < max(s.PWM[0], s.PWM[1]) {
			return err
		}
	}
	err := c.io.WritePWM(ctx, pwm)
	readback, readErr := c.io.ReadPWM(ctx)
	if readErr == nil {
		s.PWM = readback
		s.PWMAt = now
	}
	if err == nil {
		err = readErr
	}
	if err == nil && readback != [2]int{pwm, pwm} {
		err = errors.New("fan write readback mismatch")
	}
	s.WriteError = errorText(err)
	if err != nil {
		return err
	}
	s.LastChange = now.Add(time.Since(started))
	s.NextOrdinaryAt = s.LastChange.Add(holdPeriod)
	s.DesiredPWM = pwm
	if err = c.persist(s); err != nil {
		s.StateError = err.Error()
		return err
	}
	s.StateError = ""
	log.Printf("Fan PWM updated to %d: %s", pwm, s.Reason)
	return nil
}

func (c *Controller) SetMode(ctx context.Context, mode string, now time.Time) error {
	started := time.Now()
	c.op.Lock()
	defer c.op.Unlock()
	s := c.Status()
	if mode != "auto" && mode != "manual" {
		return errors.New("mode must be auto or manual")
	}
	if mode == "auto" && !s.Available {
		return errors.New("automatic control not configured")
	}
	if s.Mode == mode {
		return nil
	}
	s.Mode = mode
	if next := now.Add(time.Since(started) + holdPeriod); next.After(s.NextOrdinaryAt) {
		s.NextOrdinaryAt = next
	}
	s.Reason = "Mode changed; waiting for sample"
	if err := c.persist(&s); err != nil {
		return err
	}
	c.publish(s)
	return nil
}

func (c *Controller) SetManual(ctx context.Context, pwm int, now time.Time) error {
	started := time.Now()
	if pwm < 0 || pwm > 255 {
		return errors.New("PWM must be between 0 and 255")
	}
	c.op.Lock()
	defer c.op.Unlock()
	s := c.Status()
	if s.DryRun {
		return errors.New("hardware writes disabled in dry-run mode")
	}
	// Suspend automatic control before attempting a manual write, even if it fails.
	s.Mode = "manual"
	s.Reason = "Manual override"
	s.NextOrdinaryAt = now.Add(holdPeriod)
	if err := c.persist(&s); err != nil {
		s.StateError = err.Error()
		c.publish(s)
		return err
	}
	err := c.write(ctx, &s, pwm, now.Add(time.Since(started)))
	c.publish(s)
	return err
}

func (c *Controller) Run(ctx context.Context) {
	c.Sample(ctx, time.Now())
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Sample(ctx, time.Now())
		}
	}
}
