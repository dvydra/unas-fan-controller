package main

import (
	"math"
	"time"
)

const holdPeriod = 10 * time.Minute
const staleAfter = 90 * time.Second

type PolicyInput struct {
	Now, NextOrdinaryAt     time.Time
	Current                 int
	Ambient, Hottest        float64
	AmbientValid, DiskValid bool
}

type Decision struct {
	PWM    int
	Reason string
	Safety bool
}

func Decide(in PolicyInput) Decision {
	if in.DiskValid && in.Hottest >= 50 {
		return Decision{255, "Disk at or above 50°C", true}
	}
	if !in.AmbientValid || !in.DiskValid {
		return Decision{max(in.Current, 167), "Temperature unavailable: fallback cooling", true}
	}
	if in.Now.Before(in.NextOrdinaryAt) {
		return Decision{in.Current, "Ten-minute hold", false}
	}
	floor := 38.0
	switch {
	case in.Ambient >= 35:
		floor = 128
	case in.Ambient > 30:
		floor = 80 + (in.Ambient-30)*9.6
	case in.Ambient > 20:
		floor = 38 + (in.Ambient-20)*4.2
	}
	pwm, reason := in.Current, "Disk in target range"
	if in.Hottest > 40 {
		pwm += 16
		reason = "Disk above 40°C"
	} else if in.Hottest < 35 {
		pwm -= 8
		reason = "Disk below 35°C"
	}
	if int(math.Ceil(floor)) > pwm {
		pwm = int(math.Ceil(floor))
		reason = "Ambient minimum"
	}
	return Decision{min(255, max(38, pwm)), reason, false}
}
