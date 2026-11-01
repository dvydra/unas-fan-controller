package main

import (
	"testing"
	"time"
)

func TestPolicy(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	at := func(hour, minute int) time.Time { return time.Date(2026, 11, 2, hour, minute, 0, 0, sydney) }
	for _, tt := range []struct {
		name           string
		now            time.Time
		disk           float64
		current        int
		early, missing bool
		want           int
	}{
		{"day stock while cool", at(12, 0), 39, 38, false, false, 38},
		{"day ramps slowly above 42", at(12, 0), 42.5, 38, false, false, 42},
		{"day holds at 42", at(12, 0), 42, 50, false, false, 50},
		{"day holds at 40", at(12, 0), 40, 50, false, false, 50},
		{"day eases off below 40", at(12, 0), 39.9, 50, false, false, 46},
		{"night stock at 44", at(23, 0), 44, 38, false, false, 38},
		{"night ramps slowly above 45", at(23, 0), 45.5, 38, false, false, 42},
		{"night holds at 43", at(23, 0), 43, 60, false, false, 60},
		{"night eases off below 43", at(23, 0), 42.9, 60, false, false, 56},
		{"night starts at 20:00", at(20, 0), 44, 38, false, false, 38},
		{"day before 20:00", at(19, 59), 44, 38, false, false, 42},
		{"night until 07:00", at(6, 59), 44, 38, false, false, 38},
		{"day from 07:00", at(7, 0), 44, 38, false, false, 42},
		{"ramp stops at half speed", at(12, 0), 48, 126, false, false, 128},
		{"half speed is the ordinary ceiling", at(12, 0), 49, 128, false, false, 128},
		{"faster fan returns to the ceiling", at(12, 0), 44, 167, false, false, 128},
		{"never below stock", at(23, 0), 30, 38, false, false, 38},
		{"before hold", at(12, 0), 46, 50, true, false, 50},
		{"emergency", at(23, 0), 50, 38, true, false, 255},
		{"outage", at(23, 0), 38, 38, true, true, 167},
		{"outage never reduces", at(23, 0), 38, 200, true, true, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			due := tt.now
			if tt.early {
				due = tt.now.Add(time.Nanosecond)
			}
			got := Decide(PolicyInput{Now: tt.now, NextOrdinaryAt: due, Current: tt.current, Hottest: tt.disk, DiskValid: !tt.missing})
			if got.PWM != tt.want {
				t.Fatalf("got %d want %d (%s)", got.PWM, tt.want, got.Reason)
			}
		})
	}
}
