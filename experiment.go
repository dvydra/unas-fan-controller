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

const (
	experimentBlocks = 48
	experimentBlock  = 2 * time.Hour
	// Stock firmware held disks at 42–48°C, so the stock speed needs headroom below the 50°C emergency rule.
	experimentAbortC = 49
)

// PWM 38 is the stock firmware speed; 56 is the controller's usual ambient floor.
var experimentLevels = [2]int{38, 56}

type ExperimentConfig struct {
	ID            string `json:"id"`
	RoomURL       string `json:"room_url"`
	RoomAccessory string `json:"room_accessory"`
}

type ExperimentStatus struct {
	ID             string    `json:"id"`
	Phase          string    `json:"phase"`
	Block          int       `json:"block"`
	TotalBlocks    int       `json:"total_blocks"`
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
			e.status.TotalBlocks = experimentBlocks
			e.restartBlock("Process restarted", now)
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
		e.status = ExperimentStatus{ID: id, Block: -1, TotalBlocks: experimentBlocks, ArmedAt: now}
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

// Speeds alternate block by block and flip each day. Every adjacent pair holds both speeds at
// nearly the same ambient, and every two-hour time-of-day slot gets each speed twice over four days.
func experimentPWM(block int) int {
	return experimentLevels[(block%12+block/12)%2]
}

// restartBlock discards the current block's hold so it is captured again from the start.
// Losing one block must not cost the rest of a multi-day run.
func (e *Experiment) restartBlock(reason string, now time.Time) {
	if e.status.BlockStartedAt.IsZero() {
		return
	}
	e.status.BlockStartedAt = time.Time{}
	e.transition("running", fmt.Sprintf("%s; restarting block %d of %d", reason, e.status.Block+1, experimentBlocks), now)
}

func (e *Experiment) decide(now time.Time, s ControllerStatus, t Telemetry, telemetryErr error) *Decision {
	gap := now.Before(e.lastSeen) || now.Sub(e.lastSeen) > staleAfter
	e.lastSeen = now
	// Cached readings stay valid for staleAfter, so one failed read does not void a block.
	valid := s.WriteError == "" && s.StateError == "" && fresh(s.AmbientAt, now) && fresh(s.PWMAt, now) && len(s.Disks) > 0
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
		case hottest >= experimentAbortC:
			e.stop("aborted", fmt.Sprintf("Disk reached %d°C; current block incomplete", experimentAbortC), now, true)
		case !valid || gap:
			// Normal policy supplies fallback cooling; the block restarts once readings return.
			e.restartBlock("Readings missing", now)
			return nil
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
		e.transition("running", fmt.Sprintf("Starting block 1 of %d", experimentBlocks), now)
	}
	if e.status.Phase != "running" {
		if e.status.Recovery {
			return &Decision{max(167, max(s.PWM[0], s.PWM[1])), "Experiment aborted: fallback cooling", true}
		}
		return nil
	}
	if !e.status.BlockStartedAt.IsZero() && now.Sub(e.status.BlockStartedAt) >= experimentBlock {
		if e.status.Block == experimentBlocks-1 {
			e.transition("completed", fmt.Sprintf("All %d blocks completed; normal cooling resumed", experimentBlocks), now)
			return nil
		}
		e.status.Block++
		e.status.TargetPWM = experimentPWM(e.status.Block)
		e.status.BlockStartedAt = time.Time{}
		e.transition("running", fmt.Sprintf("Starting block %d of %d", e.status.Block+1, experimentBlocks), now)
	}
	if e.status.Phase != "running" {
		if e.status.Recovery {
			return &Decision{max(167, max(s.PWM[0], s.PWM[1])), "Experiment aborted: fallback cooling", true}
		}
		return nil
	}
	return &Decision{e.status.TargetPWM, fmt.Sprintf("Experiment block %d/%d", e.status.Block+1, experimentBlocks), false}
}

func (e *Experiment) finish(now time.Time, s *ControllerStatus, t Telemetry, telemetryErr error) {
	if e.status.Phase == "baseline" && (s.ReadError != "" || telemetryErr != nil) {
		e.status.CoolSince = time.Time{}
	}
	if s.Mode != "auto" {
		e.stop("cancelled", "Manual intervention", now, false)
	}
	if e.status.Phase == "running" && (s.WriteError != "" || s.StateError != "") {
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
		e.transition("running", fmt.Sprintf("Holding block %d of %d", e.status.Block+1, experimentBlocks), now)
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
