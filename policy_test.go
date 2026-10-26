package main

import (
	"testing"
	"time"
)

func TestPolicy(t *testing.T) {
	due := time.Unix(1000, 0)
	for _, tt := range []struct {
		name           string
		disk, ambient  float64
		current        int
		early, missing bool
		want           int
	}{
		{"cool", 34.9, 20, 80, false, false, 72},
		{"lower boundary", 35, 20, 80, false, false, 80},
		{"upper boundary", 40, 20, 80, false, false, 80},
		{"warm", 40.1, 20, 80, false, false, 96},
		{"before hold", 41, 20, 80, true, false, 80},
		{"ambient must wait", 38, 35, 38, true, false, 38},
		{"ambient middle", 38, 25, 38, false, false, 59},
		{"ambient hot", 38, 32.5, 38, false, false, 104},
		{"ambient cap", 38, 36, 38, false, false, 128},
		{"minimum", 30, 19, 38, false, false, 38},
		{"maximum", 49, 20, 250, false, false, 255},
		{"emergency", 50, 20, 38, true, false, 255},
		{"emergency beats outage", 50, 20, 38, true, true, 255},
		{"outage", 38, 20, 38, true, true, 167},
		{"outage never reduces", 38, 20, 200, true, true, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := due
			if tt.early {
				now = due.Add(-time.Nanosecond)
			}
			got := Decide(PolicyInput{Now: now, NextOrdinaryAt: due, Current: tt.current, Ambient: tt.ambient, Hottest: tt.disk, AmbientValid: !tt.missing, DiskValid: true})
			if got.PWM != tt.want {
				t.Fatalf("got %d want %d (%s)", got.PWM, tt.want, got.Reason)
			}
		})
	}
}
