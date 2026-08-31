package main

import (
	"math"
	"testing"
)

const testPaneW = 384 // 64 columns at the default 6px pitch

func newFilledGraph(samples int) *Graph {
	g := NewGraph(6, 100, 0, samples)
	for i := range samples {
		g.Push(float64(i % 100))
	}
	return g
}

func TestPinFollowsItsSampleThroughPush(t *testing.T) {
	g := newFilledGraph(64)
	g.Pin(10, testPaneW)

	want, _, ok := g.Sample(10)
	if !ok {
		t.Fatal("Sample(10) has no data in a fully filled graph")
	}
	col := g.u["u_probe"]

	g.Push(42)

	if got := g.Pinned(); got != 11 {
		t.Errorf("pin age after one sample = %d, want 11", got)
	}
	if got := g.u["u_probe"]; got != col-1 {
		t.Errorf("u_probe after push = %v, want %v (one column left)", got, col-1)
	}
	if got, _, _ := g.Sample(g.Pinned()); got != want {
		t.Errorf("pin now names sample %v, want the original %v", got, want)
	}
}

func TestPinLeavesTheRingAtTheEnd(t *testing.T) {
	g := newFilledGraph(8)
	g.Pin(7, testPaneW)
	g.Push(1)
	if g.Pinned() < g.Len() {
		t.Fatalf("pin age %d should have passed the ring depth %d", g.Pinned(), g.Len())
	}
	if _, _, ok := g.Sample(g.Pinned()); ok {
		t.Error("a sample past the ring depth should not read back")
	}
}

func TestClickAndCrosshairAgreeOnTheColumn(t *testing.T) {
	g := newFilledGraph(64)
	cols := g.cols(testPaneW)

	for _, shift := range []float64{0, 0.25, 0.5, 0.999} {
		g.u["u_shift"] = shift
		for _, frac := range []float64{0, 0.13, 0.5, 0.77, 0.99} {
			_, back, _ := g.ColumnAt(frac, testPaneW)
			g.Pin(back, testPaneW)

			wantCell := math.Floor(frac*cols + shift - 1)
			if got := g.u["u_probe"]; got != wantCell {
				t.Errorf("shift %v frac %v: crosshair on column %v, click resolved to %v",
					shift, frac, got, wantCell)
			}
		}
	}
}
