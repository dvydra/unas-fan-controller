package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Cumulative sector counters use Linux's fixed 512-byte sector unit.
// Uptime allows analysis to reject intervals spanning NAS reboot/counter reset.
func parseTelemetry(raw string, devices []string) (Telemetry, error) {
	var t Telemetry
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) < 4 {
		return t, errors.New("incomplete NAS telemetry")
	}
	for i := 0; i < 2; i++ {
		n, err := strconv.Atoi(strings.TrimSpace(lines[i]))
		if err != nil || n <= 0 || n > 20000 {
			return t, errors.New("fan RPM unavailable or invalid")
		}
		t.RPM[i] = n
	}
	uptime := strings.Fields(lines[2])
	if len(uptime) != 2 {
		return t, errors.New("invalid NAS uptime")
	}
	n, err := strconv.ParseFloat(uptime[0], 64)
	if err != nil || !plausible(n, 0, 1e12) {
		return t, errors.New("invalid NAS uptime")
	}
	t.UptimeSeconds = n
	wanted := map[string]bool{}
	for _, d := range devices {
		wanted[d] = true
	}
	found := map[string]DiskActivity{}
	for _, line := range lines[3:] {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		device := "/dev/" + f[2]
		if !wanted[device] {
			continue
		}
		if _, ok := found[device]; ok || len(f) < 14 {
			return t, errors.New("duplicate or malformed disk activity")
		}
		values := [3]uint64{}
		for i, index := range []int{5, 9, 12} {
			v, err := strconv.ParseUint(f[index], 10, 64)
			if err != nil {
				return t, err
			}
			values[i] = v
		}
		found[device] = DiskActivity{device, values[0], values[1], values[2]}
	}
	for _, d := range devices {
		v, ok := found[d]
		if !ok {
			return t, fmt.Errorf("missing disk activity: %s", d)
		}
		t.DiskActivity = append(t.DiskActivity, v)
	}
	return t, nil
}

func readTelemetry(ctx context.Context, cfg Config) (Telemetry, error) {
	room, roomErr := ReadAmbient(ctx, http.DefaultClient, cfg.Controller.Experiment.RoomURL, cfg.Controller.Experiment.RoomAccessory)
	raw, sshErr := runSSHCommand(ctx, cfg, "cat /sys/class/hwmon/hwmon0/fan1_input /sys/class/hwmon/hwmon0/fan2_input /proc/uptime /proc/diskstats")
	t, parseErr := parseTelemetry(raw, cfg.Controller.Disks)
	if roomErr == nil {
		t.Room = &room
	}
	return t, errors.Join(roomErr, sshErr, parseErr)
}
