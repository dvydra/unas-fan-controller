package main

import (
	"testing"
)

func TestParseTelemetry(t *testing.T) {
	raw := "1357\n1330\n12345.6 999.0\n8 16 sdb 11 2 201 4 12 6 301 8 0 51 10\n8 0 sda 1 2 101 4 5 6 401 8 0 71 10\n"
	s, err := parseTelemetry(raw, []string{"/dev/sda", "/dev/sdb"})
	if err != nil || s.RPM != [2]int{1357, 1330} || s.UptimeSeconds != 12345.6 || s.DiskActivity[0].ReadSectors != 101 || s.DiskActivity[1].WriteSectors != 301 || s.DiskActivity[0].IOMillis != 71 {
		t.Fatal(s, err)
	}
	for _, bad := range []string{"", "0\n1330\n5 3\n", "1357\n1330\nNaN 1\n", raw + "8 0 sda 1 2 101 4 5 6 401 8 0 71 10\n"} {
		if _, err := parseTelemetry(bad, []string{"/dev/sda", "/dev/sdb"}); err == nil {
			t.Fatal("accepted invalid metrics", bad)
		}
	}
}
