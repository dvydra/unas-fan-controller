package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type DiskReading struct {
	Device      string    `json:"device"`
	Temperature *float64  `json:"temperature,omitempty"`
	Standby     bool      `json:"standby"`
	Error       string    `json:"error,omitempty"`
	SampledAt   time.Time `json:"sampled_at"`
}

func plausible(v float64, low, high float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= low && v <= high
}

func hapType(s string) string {
	return strings.TrimLeft(strings.Split(strings.ToUpper(s), "-")[0], "0")
}

func ReadAmbient(ctx context.Context, client *http.Client, baseURL, name string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/accessories", nil)
	if err != nil {
		return 0, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("Homebridge HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, err
	}
	var payload struct {
		Accessories []struct {
			Services []struct {
				Type            string `json:"type"`
				Characteristics []struct {
					Type  string          `json:"type"`
					Value json.RawMessage `json:"value"`
				} `json:"characteristics"`
			} `json:"services"`
		} `json:"accessories"`
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return 0, err
	}
	var values []float64
	matches := 0
	for _, a := range payload.Accessories {
		match := false
		for _, s := range a.Services {
			if hapType(s.Type) != "3E" {
				continue
			}
			for _, c := range s.Characteristics {
				if hapType(c.Type) == "23" {
					var n string
					if json.Unmarshal(c.Value, &n) == nil && n == name {
						match = true
					}
				}
			}
		}
		if !match {
			continue
		}
		matches++
		for _, s := range a.Services {
			if hapType(s.Type) != "8A" && hapType(s.Type) != "BC" {
				continue
			}
			for _, c := range s.Characteristics {
				if hapType(c.Type) == "11" {
					var v *float64
					if json.Unmarshal(c.Value, &v) == nil && v != nil && plausible(*v, -20, 60) {
						values = append(values, *v)
					}
				}
			}
		}
	}
	if matches != 1 || len(values) != 1 {
		return 0, errors.New("ambient sensor missing, ambiguous, or invalid")
	}
	return values[0], nil
}

func ParseDisks(data []byte, expected []string) ([]DiskReading, error) {
	var p struct {
		Disks []DiskReading `json:"disks"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, d := range expected {
		wanted[d] = true
	}
	seen := map[string]bool{}
	var errs []error
	var valid []DiskReading
	for _, d := range p.Disks {
		if !wanted[d.Device] || seen[d.Device] {
			errs = append(errs, fmt.Errorf("unexpected or duplicate disk %s", d.Device))
			continue
		}
		seen[d.Device] = true
		if d.Error != "" || (!d.Standby && (d.Temperature == nil || !plausible(*d.Temperature, 0, 80))) || (d.Standby && d.Temperature != nil) {
			d.Temperature = nil
			if d.Error == "" {
				d.Error = "invalid temperature"
			}
			errs = append(errs, fmt.Errorf("%s: %s", d.Device, d.Error))
		}
		valid = append(valid, d)
	}
	for _, d := range expected {
		if !seen[d] {
			errs = append(errs, fmt.Errorf("missing disk %s", d))
		}
	}
	if len(expected) == 0 {
		errs = append(errs, errors.New("disk inventory empty"))
	}
	return valid, errors.Join(errs...)
}
