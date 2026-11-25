package governor

import (
	"runtime"
	"testing"
)

func fixed(l Load) func() Load { return func() Load { return l } }

func TestPermitted(t *testing.T) {
	g := &Governor{CeilingPercent: 80, HeadroomMB: 2048}
	cases := []struct {
		name string
		load Load
		want int
	}{
		{"idle", Load{Known: true, CPUPercent: 10, MemoryPercent: 30, AvailableMB: 8000}, 8},
		{"below the throttle band", Load{Known: true, CPUPercent: 59}, 8},
		{"half way down the band", Load{Known: true, CPUPercent: 70, AvailableMB: 8000}, 4},
		{"at the ceiling", Load{Known: true, CPUPercent: 95}, 1},
		{"memory short", Load{Known: true, CPUPercent: 5, MemoryPercent: 90, AvailableMB: 500}, 1},
		{"memory high but plenty free", Load{Known: true, CPUPercent: 5, MemoryPercent: 90, AvailableMB: 4000}, 8},
		{"unknown load", Load{}, 8},
	}
	for _, c := range cases {
		g.Sample = fixed(c.load)
		if got, _ := g.Permitted(8); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	off := &Governor{Sample: fixed(Load{Known: true, CPUPercent: 100})}
	if got, _ := off.Permitted(3); got != 3 {
		t.Errorf("an off governor limits nothing: %d", got)
	}
	if got, _ := g.Permitted(0); got != 1 {
		t.Errorf("never zero: %d", got)
	}
}

func TestSample(t *testing.T) {
	l := Sample()
	if runtime.GOOS != "linux" {
		if l.Known {
			t.Fatal("only Linux readings are known")
		}
		return
	}
	if !l.Known || l.MemoryPercent <= 0 || l.MemoryPercent > 100 || l.AvailableMB <= 0 {
		t.Fatalf("%+v", l)
	}
}
