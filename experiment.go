package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type ExperimentConfig struct {
	ID            string `json:"id"`
	RoomURL       string `json:"room_url"`
	RoomAccessory string `json:"room_accessory"`
}

type ExperimentStatus struct {
	ID             string    `json:"id"`
	Phase          string    `json:"phase"`
	Block          int       `json:"block"`
	TargetPWM      int       `json:"target_pwm"`
	ArmedAt        time.Time `json:"armed_at"`
	CoolSince      time.Time `json:"cool_since"`
	StartedAt      time.Time `json:"started_at"`
	BlockStartedAt time.Time `json:"block_started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Reason         string    `json:"reason"`
	Error          string    `json:"error,omitempty"`
	Recovery       bool      `json:"recovery"`
}

type DiskActivity struct {
	Device       string `json:"device"`
	ReadSectors  uint64 `json:"read_sectors"`
	WriteSectors uint64 `json:"write_sectors"`
	IOMillis     uint64 `json:"io_millis"`
}
type Telemetry struct {
	SampledAt     time.Time      `json:"sampled_at"`
	Room          *float64       `json:"room,omitempty"`
	RPM           [2]int         `json:"rpm"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	DiskActivity  []DiskActivity `json:"disk_activity"`
}
type ExperimentSample struct {
	Time           time.Time        `json:"time"`
	Controller     ControllerStatus `json:"controller"`
	Telemetry      Telemetry        `json:"telemetry"`
	TelemetryError string           `json:"telemetry_error,omitempty"`
}
type Experiment struct {
	status           ExperimentStatus
	dir              string
	lastSeen         time.Time
	lastRecorded     time.Time
	terminalRecorded bool
}

func newExperiment(parent, id string, now time.Time) (*Experiment, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`).MatchString(id) {
		return nil, errors.New("invalid experiment ID")
	}
	e := &Experiment{dir: filepath.Join(parent, "experiments", id), lastSeen: now}
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "state.json"))
	if err == nil {
		if json.Unmarshal(b, &e.status) != nil || e.status.ID != id {
			return nil, errors.New("invalid experiment state; refusing to rearm")
		}
		switch e.status.Phase {
		case "running":
			e.stop("aborted", "Process restarted; current block incomplete", now, true)
		case "baseline":
			e.status.CoolSince = time.Time{}
			e.transition("baseline", "Process restarted; baseline timer reset", now)
		case "completed", "aborted", "cancelled":
			e.terminalRecorded = true
		default:
			return nil, errors.New("invalid experiment phase")
		}
	} else if os.IsNotExist(err) {
		// Existing data without state must never silently start the same experiment again.
		for _, name := range []string{"events.jsonl", "samples.jsonl"} {
			if _, err := os.Stat(filepath.Join(e.dir, name)); !os.IsNotExist(err) {
				return nil, errors.New("experiment data exists without state")
			}
		}
		e.status = ExperimentStatus{ID: id, Block: -1, ArmedAt: now}
		e.transition("baseline", "Waiting for all disks below 40°C for thirty minutes", now)
	} else {
		return nil, err
	}
	return e, nil
}

func (e *Experiment) active() bool {
	return e.status.Phase == "baseline" || e.status.Phase == "running"
}

func (e *Experiment) save() error {
	if e.dir == "" {
		return errors.New("experiment storage unavailable")
	}
	b, err := json.Marshal(e.status)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(e.dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(e.dir, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(e.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (e *Experiment) append(name string, value any) error {
	if e.dir == "" {
		return errors.New("experiment storage unavailable")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(e.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size()+int64(len(b)+1) > 32<<20 {
		return errors.New("experiment capture reached 32 MiB file limit")
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (e *Experiment) fail(err error, now time.Time) {
	e.status.Phase = "aborted"
	e.status.Reason = "Recording or state persistence failed; experiment stopped"
	e.status.Error = err.Error()
	e.status.UpdatedAt = now
	e.status.Recovery = true
	// Best effort. If storage is unavailable, restart either sees aborted or treats running as interrupted.
	_ = e.save()
}
func (e *Experiment) transition(phase, reason string, now time.Time) {
	e.status.Phase = phase
	e.status.Reason = reason
	e.status.UpdatedAt = now
	if err := e.save(); err != nil {
		e.fail(err, now)
		return
	}
	if err := e.append("events.jsonl", struct {
		Time  time.Time        `json:"time"`
		Kind  string           `json:"kind"`
		State ExperimentStatus `json:"state"`
	}{now, "transition", e.status}); err != nil {
		e.fail(err, now)
	}
}
func (e *Experiment) stop(phase, reason string, now time.Time, recovery bool) {
	if !e.active() {
		return
	}
	e.status.Recovery = recovery
	e.transition(phase, reason, now)
}

// Each two-hour time-of-day slot receives all three speeds across three days.
func experimentPWM(block int) int {
	order := [12]int{2, 0, 1, 2, 1, 0, 1, 2, 0, 1, 0, 2}
	return [3]int{48, 56, 64}[(order[block%12]+block/12)%3]
}

func (e *Experiment) decide(now time.Time, s ControllerStatus, t Telemetry, telemetryErr error) *Decision {
	gap := now.Before(e.lastSeen) || now.Sub(e.lastSeen) > staleAfter
	e.lastSeen = now
	valid := s.ReadError == "" && s.WriteError == "" && s.StateError == "" && telemetryErr == nil && t.Room != nil && plausible(*t.Room, -20, 60) && fresh(s.AmbientAt, now) && fresh(s.PWMAt, now) && len(s.Disks) > 0
	hottest := 0.0
	for _, d := range s.Disks {
		if d.Standby || d.Temperature == nil || d.Error != "" || !fresh(d.SampledAt, now) {
			valid = false
			continue
		}
		hottest = max(hottest, *d.Temperature)
	}
	if s.Mode != "auto" {
		e.stop("cancelled", "Manual intervention", now, false)
		return nil
	}
	if e.status.Phase == "running" {
		switch {
		case hottest >= 45:
			e.stop("aborted", "Disk reached 45°C; current block incomplete", now, true)
		case !valid || gap:
			e.stop("aborted", "Missing readings or observation gap; current block incomplete", now, true)
		case s.DryRun:
			e.stop("aborted", "Dry-run enabled during experiment; current block incomplete", now, false)
		case !e.status.BlockStartedAt.IsZero() && s.PWM != [2]int{e.status.TargetPWM, e.status.TargetPWM}:
			e.stop("aborted", "Fan PWM changed outside experiment; current block incomplete", now, true)
		}
	}
	if e.status.Recovery {
		return &Decision{max(167, max(s.PWM[0], s.PWM[1])), "Experiment aborted: fallback cooling", true}
	}
	if e.status.Phase == "baseline" {
		if !valid || gap || hottest >= 40 || s.DryRun {
			e.status.CoolSince = time.Time{}
			return nil
		}
		if e.status.CoolSince.IsZero() {
			e.status.CoolSince = now
		}
		if now.Sub(e.status.CoolSince) < 30*time.Minute || now.Before(s.NextOrdinaryAt) {
			return nil
		}
		e.status.StartedAt = now
		e.status.Block = 0
		e.status.TargetPWM = experimentPWM(0)
		e.transition("running", "Starting block 1 of 36", now)
	}
	if e.status.Phase != "running" {
		if e.status.Recovery {
			return &Decision{max(167, max(s.PWM[0], s.PWM[1])), "Experiment aborted: fallback cooling", true}
		}
		return nil
	}
	if !e.status.BlockStartedAt.IsZero() && now.Sub(e.status.BlockStartedAt) >= 2*time.Hour {
		if e.status.Block == 35 {
			e.transition("completed", "All 36 blocks completed; normal cooling resumed", now)
			return nil
		}
		e.status.Block++
		e.status.TargetPWM = experimentPWM(e.status.Block)
		e.status.BlockStartedAt = time.Time{}
		e.transition("running", fmt.Sprintf("Starting block %d of 36", e.status.Block+1), now)
	}
	if e.status.Phase != "running" {
		if e.status.Recovery {
			return &Decision{max(167, max(s.PWM[0], s.PWM[1])), "Experiment aborted: fallback cooling", true}
		}
		return nil
	}
	return &Decision{e.status.TargetPWM, fmt.Sprintf("Experiment block %d/36", e.status.Block+1), false}
}

func (e *Experiment) finish(now time.Time, s *ControllerStatus, t Telemetry, telemetryErr error) {
	if e.status.Phase == "baseline" && (s.ReadError != "" || telemetryErr != nil) {
		e.status.CoolSince = time.Time{}
	}
	if s.Mode != "auto" {
		e.stop("cancelled", "Manual intervention", now, false)
	}
	if e.status.Phase == "running" && (s.ReadError != "" || s.WriteError != "" || s.StateError != "") {
		e.stop("aborted", "Read/write failure; current block incomplete", now, true)
	}
	if e.status.Recovery && s.PWM[0] >= 167 && s.PWM[1] >= 167 && s.WriteError == "" && e.status.Error == "" {
		e.status.Recovery = false
		if err := e.save(); err != nil {
			e.fail(err, now)
		}
	}
	if e.status.Phase == "running" && e.status.BlockStartedAt.IsZero() && s.PWM == [2]int{e.status.TargetPWM, e.status.TargetPWM} && s.WriteError == "" {
		e.status.BlockStartedAt = now
		if s.LastChange.After(now) {
			e.status.BlockStartedAt = s.LastChange
		}
		e.transition("running", fmt.Sprintf("Holding block %d of 36", e.status.Block+1), now)
	}
	status := e.status
	s.Experiment = &status
	// Use UTC minute buckets so small polling jitter does not skip every other minute.
	if e.terminalRecorded || (e.active() && !e.lastRecorded.IsZero() && !now.Truncate(time.Minute).After(e.lastRecorded.Truncate(time.Minute))) {
		return
	}
	if err := e.append("samples.jsonl", ExperimentSample{now, *s, t, errorText(telemetryErr)}); err != nil {
		e.fail(err, now)
	}
	e.lastRecorded = now
	e.terminalRecorded = !e.active()
	status = e.status
	s.Experiment = &status
}

func (e *Experiment) fanEvent(now time.Time, s ControllerStatus, requested int, err error) {
	if !e.active() && !e.status.Recovery && e.terminalRecorded {
		return
	}
	if logErr := e.append("events.jsonl", struct {
		Time      time.Time `json:"time"`
		Kind      string    `json:"kind"`
		Requested int       `json:"requested_pwm"`
		Observed  [2]int    `json:"observed_pwm"`
		Reason    string    `json:"reason"`
		Error     string    `json:"error,omitempty"`
	}{now, "fan-write", requested, s.PWM, s.Reason, errorText(err)}); logErr != nil {
		e.fail(logErr, now)
	}
	if err != nil {
		e.stop("aborted", "Fan write failed; current block incomplete", now, true)
	}
}
