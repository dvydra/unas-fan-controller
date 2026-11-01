package main

import (
	"fmt"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo
)

const holdPeriod = 10 * time.Minute
const staleAfter = 90 * time.Second

const (
	stockPWM = 38 // the UNAS firmware's fixed speed, about 700 rpm
	// About half speed. Anything faster is too loud for ordinary cooling; only the emergency exceeds it.
	quietCeiling   = 128
	rampStep       = 4
	dayThreshold   = 42.0
	nightThreshold = 45.0
	easeBelow      = 2.0 // ease off only once this far under the threshold
	nightStart     = 20
	nightEnd       = 7
)

var quietZone = mustLoadLocation("Australia/Sydney")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

type PolicyInput struct {
	Now, NextOrdinaryAt time.Time
	Current             int
	Hottest             float64
	DiskValid           bool
}

type Decision struct {
	PWM    int
	Reason string
	Safety bool
}

// threshold is the hottest-disk temperature above which the fan ramps: lower by day, stock-like at night.
func threshold(now time.Time) float64 {
	if h := now.In(quietZone).Hour(); h >= nightStart || h < nightEnd {
		return nightThreshold
	}
	return dayThreshold
}

func Decide(in PolicyInput) Decision {
	if in.DiskValid && in.Hottest >= 50 {
		return Decision{255, "Disk at or above 50°C", true}
	}
	if !in.DiskValid {
		return Decision{max(in.Current, 167), "Temperature unavailable: fallback cooling", true}
	}
	if in.Now.Before(in.NextOrdinaryAt) {
		return Decision{in.Current, "Ten-minute hold", false}
	}
	limit := threshold(in.Now)
	pwm, reason := in.Current, "Disk in target range"
	if in.Hottest > limit {
		pwm += rampStep
		reason = fmt.Sprintf("Disk above %.0f°C", limit)
	} else if in.Hottest < limit-easeBelow {
		pwm -= rampStep
		reason = fmt.Sprintf("Disk below %.0f°C", limit-easeBelow)
	}
	if pwm >= quietCeiling {
		pwm = quietCeiling
		reason = "Half-speed ceiling"
	}
	if pwm <= stockPWM {
		pwm = stockPWM
		reason = "Stock speed"
	}
	return Decision{pwm, reason, false}
}
