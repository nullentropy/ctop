package sys

import (
	"sort"
	"strings"

	"github.com/shirou/gopsutil/v4/sensors"
)

type TempClass int

const (
	TempOther TempClass = iota
	TempCPU
	TempGPU
	TempVRM
	TempMem
	TempDisk
	TempBattery
	TempSys
)

func (c TempClass) String() string {
	switch c {
	case TempCPU:
		return "cpu"
	case TempGPU:
		return "gpu"
	case TempVRM:
		return "vrm"
	case TempMem:
		return "mem"
	case TempDisk:
		return "ssd"
	case TempBattery:
		return "batt"
	case TempSys:
		return "sys"
	}
	return "misc"
}

var tempOrder = []TempClass{TempCPU, TempGPU, TempVRM, TempMem, TempDisk, TempBattery, TempSys, TempOther}

type Temp struct {
	Label string
	Class TempClass
	C     float64
	High  float64
	Crit  float64
}

var tempRules = []struct {
	class TempClass
	match []string
}{
	{TempVRM, []string{"vrm", "vcore", "vr_", "regulator", "pmu tpd", "tmvr", "tv0r", "tv1r", "tv2r"}},
	{TempGPU, []string{"gpu", "nouveau", "radeon", "nvidia", "tg0"}},
	{TempDisk, []string{"nvme", "nand", "ssd", "drive", "th0p"}},
	{TempBattery, []string{"battery", "gas gauge", "bat0", "bat1"}},
	{TempCPU, []string{"coretemp", "k10temp", "zenpower", "cpu", "package_id", "core_", "tdie", "tctl", "tccd", "pacc", "eacc", "soc", "tc0"}},
	{TempMem, []string{"dimm", "spd5118", "_mem", "tm0"}},
	{TempSys, []string{"acpitz", "pch", "systin", "ambient", "enclosure", "chipset", "board", "wifi", "thunderbolt", "pmu t", "ta0", "ta1", "tb0", "tn0", "ti0", "tw0"}},
}

var tempSkip = []string{"tcal"}

func skipTemp(key string) bool {
	k := strings.ToLower(key)
	for _, m := range tempSkip {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

func classifyTemp(key string) TempClass {
	k := strings.ToLower(key)
	for _, r := range tempRules {
		for _, m := range r.match {
			if strings.Contains(k, m) {
				return r.class
			}
		}
	}
	return TempOther
}

func readTemps() []Temp {
	ss, _ := sensors.SensorsTemperatures()

	out := make([]Temp, 0, len(ss))
	for _, s := range ss {
		if s.Temperature <= 1 || s.Temperature > 150 || skipTemp(s.SensorKey) {
			continue
		}
		out = append(out, Temp{
			Label: s.SensorKey,
			Class: classifyTemp(s.SensorKey),
			C:     s.Temperature,
			High:  s.High,
			Crit:  s.Critical,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].C > out[j].C })
	return out
}

func GroupTemps(ts []Temp) []Temp {
	hottest := make(map[TempClass]Temp, len(tempOrder))
	for _, t := range ts {
		if cur, ok := hottest[t.Class]; !ok || t.C > cur.C {
			hottest[t.Class] = t
		}
	}
	out := make([]Temp, 0, len(hottest))
	for _, c := range tempOrder {
		if t, ok := hottest[c]; ok {
			out = append(out, t)
		}
	}
	return out
}
