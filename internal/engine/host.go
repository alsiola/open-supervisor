package engine

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/alsiola/open-supervisor/internal/config"
	hostinfo "github.com/alsiola/open-supervisor/internal/host"
	"github.com/alsiola/open-supervisor/internal/logx"
)

// recoverAt is the share of a threshold a reading must fall back under before it counts as recovered, so one
// hovering at the line doesn't raise an alert every reading.
const recoverAt = 0.95

// hostWatch remembers which of the host's readings are over their thresholds.
type hostWatch struct {
	mu   sync.Mutex
	over map[string]bool
}

// hostAlert is a reading crossing its threshold: going over it, or back under.
type hostAlert struct {
	Metric    string // load, memory or disk
	Path      string // the disk's path
	Value     float64
	Threshold float64
	Over      bool
}

// update compares a reading with the thresholds and returns what crossed since the last.
func (w *hostWatch) update(s hostinfo.Sample, h config.Host) []hostAlert {
	load, memory, disk := h.Thresholds()
	type reading struct {
		key, metric, path string
		value, threshold  float64
	}
	var rs []reading
	if s.HasLoad {
		rs = append(rs, reading{"load", "load", "", s.LoadPerCPU(), load})
	}
	if s.MemTotal > 0 {
		rs = append(rs, reading{"memory", "memory", "", s.MemoryUsedPct(), memory})
	}
	for _, d := range s.Disks {
		rs = append(rs, reading{"disk:" + d.Path, "disk", d.Path, d.UsedPct(), disk})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over == nil {
		w.over = map[string]bool{}
	}
	var out []hostAlert
	for _, r := range rs {
		was := w.over[r.key]
		now := r.value > r.threshold || (was && r.value > r.threshold*recoverAt)
		if now != was {
			w.over[r.key] = now
			out = append(out, hostAlert{Metric: r.metric, Path: r.path, Value: r.value, Threshold: r.threshold, Over: now})
		}
	}
	return out
}

// Over lists the readings over their thresholds now, e.g. "disk / 93%".
func (w *hostWatch) Over(s hostinfo.Sample) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	if w.over["load"] {
		out = append(out, fmt.Sprintf("load %.1f/cpu", s.LoadPerCPU()))
	}
	if w.over["memory"] {
		out = append(out, fmt.Sprintf("memory %d%%", int(math.Round(s.MemoryUsedPct()))))
	}
	for _, d := range s.Disks {
		if w.over["disk:"+d.Path] {
			out = append(out, fmt.Sprintf("disk %s %d%%", d.Path, int(math.Round(d.UsedPct()))))
		}
	}
	return out
}

func (a hostAlert) String() string {
	what := a.Metric
	if a.Path != "" {
		what += " " + a.Path
	}
	value, threshold := fmt.Sprintf("%.0f%%", a.Value), fmt.Sprintf("%.0f%%", a.Threshold)
	if a.Metric == "load" {
		value, threshold = fmt.Sprintf("%.2f per CPU", a.Value), fmt.Sprintf("%.2f", a.Threshold)
	}
	if a.Over {
		return fmt.Sprintf("host: %s is at %s, over the %s threshold", what, value, threshold)
	}
	return fmt.Sprintf("host: %s is back to %s, under the %s threshold", what, value, threshold)
}

// hostPaths are the paths whose disks are watched: the worktrees', the state folder's, every project's
// checkout and any the config adds.
func (s *Supervisor) hostPaths() []string {
	paths := []string{s.data, s.state}
	for _, p := range s.Projects() {
		paths = append(paths, p.Root)
	}
	for _, d := range s.settings().Host.Disks {
		paths = append(paths, config.ExpandHome(d))
	}
	return paths
}

// readHost takes a reading of the host, sends it to telemetry and raises an alert for each threshold crossed.
func (s *Supervisor) readHost() {
	h := s.settings().Host
	sample, err := hostinfo.Read(s.hostPaths())
	if err != nil {
		warnOnce("host: couldn't read the machine's load, memory or disk: " + logx.ErrText(err))
		return
	}
	s.mu.Lock()
	s.hostSample = &sample
	s.mu.Unlock()
	s.capture("host_metrics", hostProps(sample))
	for _, a := range s.host.update(sample, h) {
		logx.Default.Printf("%s", a)
		props := map[string]any{"metric": a.Metric, "value": a.Value, "threshold": a.Threshold, "over": a.Over}
		if a.Path != "" {
			props["path"] = a.Path
		}
		s.capture("host_alert", props)
	}
	s.changed()
}

// HostAlerts are the host's readings over their thresholds now; none when the watch is off.
func (s *Supervisor) HostAlerts() []string {
	s.mu.RLock()
	sample := s.hostSample
	s.mu.RUnlock()
	if sample == nil {
		return nil
	}
	return s.host.Over(*sample)
}

// hostProps are a reading's telemetry properties. Each disk is its own set, by path, plus the fullest one's
// under disk_*, so one property can be charted and alerted on however many disks there are.
func hostProps(s hostinfo.Sample) map[string]any {
	props := map[string]any{"cpus": s.CPUs}
	if s.HasLoad {
		props["load_1"], props["load_5"], props["load_15"] = s.Load[0], s.Load[1], s.Load[2]
		props["load_per_cpu"] = s.LoadPerCPU()
	}
	if s.MemTotal > 0 {
		props["memory_total_bytes"] = s.MemTotal
		props["memory_available_bytes"] = s.MemAvailable
		props["memory_used_pct"] = s.MemoryUsedPct()
	}
	disks := append([]hostinfo.Disk(nil), s.Disks...)
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].UsedPct() > disks[j].UsedPct() })
	var list []map[string]any
	for _, d := range disks {
		list = append(list, map[string]any{"path": d.Path, "total_bytes": d.Total, "free_bytes": d.Free, "used_pct": d.UsedPct()})
	}
	if len(disks) > 0 {
		props["disk_path"] = disks[0].Path
		props["disk_total_bytes"] = disks[0].Total
		props["disk_free_bytes"] = disks[0].Free
		props["disk_used_pct"] = disks[0].UsedPct()
		props["disks"] = list
	}
	return props
}
