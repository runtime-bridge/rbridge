// Package governor decides how many sessions may run at once from how busy
// the machine is. Each session is a runtime process doing real work, and on a
// machine that is also running everything else, "as many as configured" is
// how the box ends up swapping or killing something.
//
// The limit is derived, not just configured: below 75% of the ceiling every
// configured slot is available; between there and the ceiling the number
// ramps down linearly; at the ceiling it is one. It is never zero, so work
// already in the system drains instead of deadlocking behind itself.
package governor

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// ThrottleFrom is where throttling starts, as a share of the ceiling.
const ThrottleFrom = 0.75

// Load is one reading of the machine.
type Load struct {
	Known         bool    `json:"known"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	AvailableMB   float64 `json:"available_mb"`
}

// Governor holds the ceiling and reads the load.
type Governor struct {
	// CeilingPercent is the share of the machine sessions may push it to.
	// Zero turns the governor off.
	CeilingPercent float64
	// HeadroomMB is the free memory below which memory starts to count.
	// Above it, a high percentage on a large shared machine still leaves
	// plenty of room, and throttling on it would only slow work down.
	HeadroomMB float64
	// Sample reads the load; tests replace it.
	Sample func() Load
}

// New returns a governor that reads this machine.
func New(ceilingPercent, headroomMB float64) *Governor {
	return &Governor{CeilingPercent: ceilingPercent, HeadroomMB: headroomMB, Sample: Sample}
}

// Enabled reports whether the governor can limit anything.
func (g *Governor) Enabled() bool { return g != nil && g.CeilingPercent > 0 }

// Peak is the binding constraint of a reading: CPU always, memory only while
// free memory is short.
func (g *Governor) Peak(l Load) float64 {
	if g.HeadroomMB > 0 && l.AvailableMB >= g.HeadroomMB {
		return l.CPUPercent
	}
	return math.Max(l.CPUPercent, l.MemoryPercent)
}

// Factor is how much of its configured capacity rbridge may use for this
// reading, from 0 to 1.
func (g *Governor) Factor(l Load) float64 {
	if !g.Enabled() || !l.Known {
		return 1
	}
	peak, ceiling := g.Peak(l), g.CeilingPercent
	start := ceiling * ThrottleFrom
	switch {
	case peak <= start:
		return 1
	case peak >= ceiling:
		return 0
	}
	return (ceiling - peak) / (ceiling - start)
}

// Permitted is how many of limit sessions may run now. Never less than one.
func (g *Governor) Permitted(limit int) (int, Load) {
	if limit < 1 {
		limit = 1
	}
	if !g.Enabled() {
		return limit, Load{}
	}
	l := g.Sample()
	n := int(math.Round(float64(limit) * g.Factor(l)))
	return max(1, min(limit, n)), l
}

// Sample reads CPU and memory use. CPU is measured since the previous call,
// so the first reading is zero, which errs towards allowing work. Outside
// Linux the load is reported as unknown and nothing is throttled.
func Sample() Load {
	if runtime.GOOS != "linux" {
		return Load{}
	}
	total, avail, ok := meminfo()
	if !ok {
		return Load{}
	}
	return Load{
		Known:         true,
		CPUPercent:    cpuPercent(),
		MemoryPercent: 100 * (1 - avail/total),
		AvailableMB:   avail / 1024,
	}
}

func meminfo() (totalKB, availKB float64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}
	return totalKB, availKB, totalKB > 0
}

var (
	cpuMu               sync.Mutex
	lastBusy, lastTotal float64
)

func cpuPercent() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var total, idle float64
	for i, f := range fields[1:] {
		v, _ := strconv.ParseFloat(f, 64)
		total += v
		if i == 3 || i == 4 { // idle, iowait
			idle += v
		}
	}
	busy := total - idle
	cpuMu.Lock()
	defer cpuMu.Unlock()
	dBusy, dTotal := busy-lastBusy, total-lastTotal
	first := lastTotal == 0
	lastBusy, lastTotal = busy, total
	if first || dTotal <= 0 {
		return 0
	}
	return math.Max(0, math.Min(100, 100*dBusy/dTotal))
}
