package sandbox

import (
	"strings"
	"testing"
)

func TestResourceLimitsResolve(t *testing.T) {
	sbx := ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 8000, MemoryMB: 12288}, CPUStepMilli: 1000}
	k8s := ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 8000, MemoryMB: 12288}, CPUStepMilli: 250}
	cases := []struct {
		name  string
		l     ResourceLimits
		in    Resources
		want  Resources
		fails string
	}{
		{"zero is the default", sbx, Resources{}, sbx.Default, ""},
		{"memory only keeps the default CPU", sbx, Resources{MemoryMB: 4096}, Resources{CPUMilli: 1000, MemoryMB: 4096}, ""},
		{"whole CPUs on SBX", sbx, Resources{CPUMilli: 2000}, Resources{CPUMilli: 2000, MemoryMB: 1536}, ""},
		{"no fraction on SBX", sbx, Resources{CPUMilli: 500}, Resources{}, "multiple of 1 CPU"},
		{"fraction on Kubernetes", k8s, Resources{CPUMilli: 500}, Resources{CPUMilli: 500, MemoryMB: 1536}, ""},
		{"off-step fraction", k8s, Resources{CPUMilli: 300}, Resources{}, "multiple of 0.25 CPUs"},
		{"memory step", sbx, Resources{MemoryMB: 1000}, Resources{}, "multiple of 512"},
		{"over the ceiling", sbx, Resources{MemoryMB: 16384}, Resources{}, "at most 8 CPUs · 12 GiB"},
		{"the ceiling itself", sbx, sbx.Max, sbx.Max, ""},
		{"an unstepped default is accepted", ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1000}, Max: Resources{CPUMilli: 1000, MemoryMB: 2048}, CPUStepMilli: 1000}, Resources{}, Resources{CPUMilli: 1000, MemoryMB: 1000}, ""},
	}
	for _, c := range cases {
		got, err := c.l.Resolve(c.in)
		if c.fails != "" {
			if err == nil || !strings.Contains(err.Error(), c.fails) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.fails)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}

func TestResourceLimitsValidate(t *testing.T) {
	ok := ResourceLimits{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 4000, MemoryMB: 8192}, CPUStepMilli: 1000}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []ResourceLimits{
		{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 1000, MemoryMB: 1024}, CPUStepMilli: 1000},
		{Default: Resources{CPUMilli: 500, MemoryMB: 1536}, Max: Resources{CPUMilli: 1000, MemoryMB: 1536}, CPUStepMilli: 1000},
		{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 1000, MemoryMB: 1536}, CPUStepMilli: 300},
		{Default: Resources{CPUMilli: 1000, MemoryMB: 1536}, Max: Resources{CPUMilli: 1000, MemoryMB: 131072}, CPUStepMilli: 1000},
		{Default: Resources{CPUMilli: 1000, MemoryMB: 256}, Max: Resources{CPUMilli: 1000, MemoryMB: 1536}, CPUStepMilli: 1000},
	}
	for i, l := range bad {
		if l.Validate() == nil {
			t.Errorf("limits %d accepted: %+v", i, l)
		}
	}
}

func TestResourceStringsAndParsing(t *testing.T) {
	for in, want := range map[Resources]string{{CPUMilli: 1000, MemoryMB: 1536}: "1 CPU · 1536 MiB", {CPUMilli: 2000, MemoryMB: 4096}: "2 CPUs · 4 GiB", {CPUMilli: 500, MemoryMB: 512}: "0.5 CPUs · 512 MiB"} {
		if got := in.String(); got != want {
			t.Errorf("%+v: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"4g": 4096, "4G": 4096, "2048m": 2048, "1536": 1536, " 1GiB ": 1024, "512mib": 512} {
		if got, err := ParseMemoryMB(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1g", "0"} {
		if _, err := ParseMemoryMB(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
	for in, want := range map[float64]int{1: 1000, 0.5: 500, 2.25: 2250, 0: 0, -1: 0, 1e9: 0} {
		if got := CPUMilli(in); got != want {
			t.Errorf("CPUMilli(%v) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int]int{0: 1, 1000: 1, 1500: 2, 2000: 2} {
		if got := CPUsFromSpec(in); got != want {
			t.Errorf("CPUsFromSpec(%d) = %d, want %d", in, got, want)
		}
	}
}

// The SBX create line takes the spec's size, whole CPUs, the worker's
// default memory when the spec has none.
func TestSBXCreateArgsUseTheSpecSize(t *testing.T) {
	d := &sbxRuntime{worker: &Worker{MemoryMB: 1536, Template: "img"}}
	args, err := d.createArgs("wc-1", Resources{CPUMilli: 2000, MemoryMB: 4096}, "img")
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(args, " "); !strings.Contains(line, "--cpus 2 --memory 4096m --template img --deny-network ** --no-share-skills") {
		t.Fatal(line)
	}
	args, _ = d.createArgs("wc-1", Resources{}, "img")
	if line := strings.Join(args, " "); !strings.Contains(line, "--cpus 1 --memory 1536m") {
		t.Fatal(line)
	}
	if _, err = d.createArgs("wc-1", Resources{MemoryMB: 256}, "img"); err == nil {
		t.Fatal("256 MiB accepted")
	}
}
