package main

import (
	"fmt"
	"time"
)

func humanBytes(b uint64) string { return humanF(float64(b), "") }

func humanRate(bps float64) string { return humanF(bps, "/s") }

func humanF(v float64, suffix string) string {
	const k = 1024
	switch {
	case v < k:
		return fmt.Sprintf("%.0f B%s", v, suffix)
	case v < k*k:
		return fmt.Sprintf("%.1f K%s", v/k, suffix)
	case v < k*k*k:
		return fmt.Sprintf("%.1f M%s", v/(k*k), suffix)
	case v < k*k*k*k:
		return fmt.Sprintf("%.2f G%s", v/(k*k*k), suffix)
	default:
		return fmt.Sprintf("%.2f T%s", v/(k*k*k*k), suffix)
	}
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d", days, h, m)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, int(d.Seconds())%60)
}
