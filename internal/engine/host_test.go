package engine

import (
	"testing"

	"github.com/alsiola/open-supervisor/internal/config"
	hostinfo "github.com/alsiola/open-supervisor/internal/host"
)

func TestHostWatchAlertsOnCrossingOnly(t *testing.T) {
	var w hostWatch
	h := config.Host{Minutes: 1, Disk: 80}
	at := func(diskPct, memPct float64) hostinfo.Sample {
		return hostinfo.Sample{
			CPUs: 2, Load: [3]float64{1, 1, 1}, HasLoad: true,
			MemTotal: 100, MemAvailable: uint64(100 - memPct),
			Disks: []hostinfo.Disk{{Path: "/data", Total: 100, Free: uint64(100 - diskPct)}},
		}
	}
	steps := []struct {
		disk, mem float64
		want      []string
	}{
		{50, 50, nil},
		{85, 50, []string{"disk /data over"}},
		{90, 50, nil}, // still over: no new alert
		{78, 50, nil}, // under 80 but not under 95% of it: still over
		{70, 95, []string{"memory over", "disk /data under"}}, // memory's default threshold is 90
		{70, 50, []string{"memory under"}},
	}
	for i, st := range steps {
		s := at(st.disk, st.mem)
		var got []string
		for _, a := range w.update(s, h) {
			g := a.Metric
			if a.Path != "" {
				g += " " + a.Path
			}
			if a.Over {
				g += " over"
			} else {
				g += " under"
			}
			got = append(got, g)
		}
		if len(got) != len(st.want) {
			t.Fatalf("step %d: alerts %v, want %v", i, got, st.want)
		}
		for j := range got {
			if got[j] != st.want[j] {
				t.Fatalf("step %d: alerts %v, want %v", i, got, st.want)
			}
		}
	}
	if over := w.Over(at(85, 50)); len(over) != 0 {
		t.Errorf("over %v after everything recovered", over)
	}
}

func TestHostPropsPutTheFullestDiskFirst(t *testing.T) {
	p := hostProps(hostinfo.Sample{CPUs: 1, Disks: []hostinfo.Disk{
		{Path: "/a", Total: 100, Free: 50},
		{Path: "/b", Total: 100, Free: 5},
	}})
	if p["disk_path"] != "/b" || p["disk_used_pct"] != 95.0 {
		t.Errorf("props %v", p)
	}
	if _, ok := p["load_1"]; ok {
		t.Error("load sent with no load read")
	}
	if _, ok := p["memory_used_pct"]; ok {
		t.Error("memory sent with no memory read")
	}
}
