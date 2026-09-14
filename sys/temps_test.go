package sys

import "testing"

func TestClassifyTemp(t *testing.T) {
	cases := []struct {
		key  string
		want TempClass
	}{
		// darwin, apple silicon (IOHID)
		{"PMU tdie4", TempCPU},
		{"PMU tdev3", TempSys},
		{"PMU TP1g", TempSys},
		{"pACC MTR Temp Sensor1", TempCPU},
		{"eACC MTR Temp Sensor0", TempCPU},
		{"SOC MTR Temp Sensor0", TempCPU},
		{"GPU MTR Temp Sensor0", TempGPU},
		{"NAND CH0 temp", TempDisk},
		{"gas gauge battery", TempBattery},
		// darwin, intel (SMC keys)
		{"TC0D", TempCPU},
		{"TG0P", TempGPU},
		{"TM0P", TempMem},
		{"TA0P", TempSys},
		{"TMVR", TempVRM},
		// linux hwmon
		{"coretemp_package_id_0", TempCPU},
		{"k10temp_tctl", TempCPU},
		{"amdgpu_edge", TempGPU},
		{"amdgpu_mem", TempGPU}, // a GPU before it is memory
		{"nouveau", TempGPU},
		{"nvme_composite", TempDisk},
		{"nct6798_vrm", TempVRM},
		{"acpitz", TempSys},
		{"iwlwifi_1", TempSys},
		{"BAT0", TempBattery},
		{"something_unheard_of", TempOther},
	}
	for _, c := range cases {
		if got := classifyTemp(c.key); got != c.want {
			t.Errorf("classifyTemp(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestGroupTemps(t *testing.T) {
	in := []Temp{
		{Label: "tdie0", Class: TempCPU, C: 45},
		{Label: "nand", Class: TempDisk, C: 34},
		{Label: "tdie1", Class: TempCPU, C: 52},
		{Label: "tdev1", Class: TempSys, C: 40},
		{Label: "tdie2", Class: TempCPU, C: 48},
	}
	got := GroupTemps(in)

	want := []struct {
		class TempClass
		label string
	}{
		{TempCPU, "tdie1"},
		{TempDisk, "nand"},
		{TempSys, "tdev1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Class != w.class || got[i].Label != w.label {
			t.Errorf("row %d = %v/%s, want %v/%s", i, got[i].Class, got[i].Label, w.class, w.label)
		}
	}
}

func TestGroupTempsEmpty(t *testing.T) {
	if got := GroupTemps(nil); len(got) != 0 {
		t.Errorf("GroupTemps(nil) = %v, want empty", got)
	}
}
