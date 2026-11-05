package collectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Same layout as borealis-36: several hwmon chips share a name (one mlx5 per
// NIC port, one coretemp per socket), one has no name file (ACPI power meter).
func TestTemperatureSameChipNames(t *testing.T) {
	sys := t.TempDir()
	devs := filepath.Join(sys, "devices")
	mk := func(hw, name, device string, temps map[string]string) {
		dir := filepath.Join(sys, "class", "hwmon", hw)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if name != "" {
			os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o644)
		}
		os.MkdirAll(filepath.Join(devs, device), 0o755)
		if err := os.Symlink(filepath.Join(devs, device), filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
		for base, label := range temps {
			os.WriteFile(filepath.Join(dir, base+"_input"), []byte("60000\n"), 0o644)
			if label != "" {
				os.WriteFile(filepath.Join(dir, base+"_label"), []byte(label+"\n"), 0o644)
			}
		}
	}
	mk("hwmon0", "mlx5", "0000:18:00.0", map[string]string{"temp1": "asic", "temp2": "module0"})
	mk("hwmon1", "mlx5", "0000:1a:00.0", map[string]string{"temp1": "asic", "temp2": "module0"})
	mk("hwmon14", "coretemp", "coretemp.0", map[string]string{"temp1": "Package id 0", "temp2": "Core 8"})
	mk("hwmon15", "coretemp", "coretemp.1", map[string]string{"temp1": "Package id 1", "temp2": "Core 8"})
	mk("hwmon16", "", "ACPI000D:00", map[string]string{"temp1": "x", "temp2": "x"}) // same label twice
	t.Setenv("HOST_SYS", sys)

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewTemperatureCollector())
	mfs, err := reg.Gather() // fails on duplicate series, as the HTTP handler did
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	devices := map[string]bool{}
	n := 0
	for _, mf := range mfs {
		if mf.GetName() != "hpc_temperature_celsius" {
			continue
		}
		for _, m := range mf.GetMetric() {
			n++
			for _, l := range m.GetLabel() {
				if l.GetName() == "device" {
					devices[l.GetValue()] = true
				}
			}
		}
	}
	if n != 10 {
		t.Errorf("want 10 temperature series, got %d", n)
	}
	for _, d := range []string{"0000:18:00.0", "0000:1a:00.0", "coretemp.0", "coretemp.1", "ACPI000D:00"} {
		if !devices[d] {
			t.Errorf("missing device=%q (have %v)", d, devices)
		}
	}
}
