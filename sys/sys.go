package sys

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type Static struct {
	Hostname string
	Platform string // "darwin 26.5"
	Kernel   string
	CPUModel string
	Cores    int // logical
	Physical int
	BootTime time.Time
}

type Fast struct {
	Time     time.Time
	Uptime   time.Duration
	CPUTotal float64   // percent, 0..100
	CPUCores []float64 // percent per logical core
	Load     [3]float64

	MemTotal, MemUsed, MemAvail  uint64
	MemWired, MemActive, MemComp uint64
	SwapTotal, SwapUsed          uint64

	NetRx, NetTx   float64
	NetRxTotal     uint64
	NetTxTotal     uint64
	NetIface       string
	DiskRead       float64
	DiskWrite      float64
	DiskReadTotal  uint64
	DiskWriteTotal uint64
}

type Slow struct {
	Time  time.Time
	Procs []Proc
	Disks []Disk
	Temps []Temp
}

// Proc is one row of the process table. CPU is instantaneous (delta over the
// poll interval), not the since-launch average gopsutil reports by default.
//
// Process state is deliberately absent: on darwin gopsutil's Status() costs
// ~2.3ms per process (1.3s for a typical 560-process machine, thirty times
// every other field combined), which would make the scan slower than the
// interval it runs on. Everything kept here totals ~40ms.
type Proc struct {
	PID     int32
	PPID    int32
	User    string
	Name    string
	Command string
	CPU     float64 // percent of one core; >100 for multithreaded
	MemPct  float64
	RSS     uint64
	Threads int32
}

// Disk is a mounted filesystem worth showing (see keepMount).
type Disk struct {
	Mount   string
	Device  string
	FSType  string
	Total   uint64
	Used    uint64
	Percent float64
}

// Collector owns the polling goroutines. Both channels are buffered with depth
// one and published non-blockingly, so a slow consumer drops stale samples
// instead of stalling collection.
type Collector struct {
	Static Static
	Fast   <-chan Fast
	Slow   <-chan Slow
}

// Start launches collection and returns as soon as the static info is read.
// Cancelling ctx stops both pollers.
func Start(ctx context.Context, fastEvery, slowEvery time.Duration) (*Collector, error) {
	st, err := readStatic()
	if err != nil {
		return nil, err
	}
	fast := make(chan Fast, 1)
	slow := make(chan Slow, 1)
	go pollFast(ctx, fastEvery, st, fast)
	go pollSlow(ctx, slowEvery, slow)
	return &Collector{Static: st, Fast: fast, Slow: slow}, nil
}

func readStatic() (Static, error) {
	var st Static
	hi, err := host.Info()
	if err != nil {
		return st, err
	}
	st.Hostname = strings.TrimSuffix(hi.Hostname, ".local")
	st.Platform = hi.Platform + " " + hi.PlatformVersion
	st.Kernel = hi.KernelVersion
	st.BootTime = time.Unix(int64(hi.BootTime), 0)
	st.Cores, _ = cpu.Counts(true)
	st.Physical, _ = cpu.Counts(false)
	// On darwin cpu.Info returns a single aggregate entry, not one per core.
	if ci, err := cpu.Info(); err == nil && len(ci) > 0 {
		st.CPUModel = ci[0].ModelName
	}
	if st.CPUModel == "" {
		st.CPUModel = "unknown CPU"
	}
	return st, nil
}

func pollFast(ctx context.Context, every time.Duration, st Static, out chan Fast) {
	cpu.Percent(0, true) // prime the delta so the first sample is real

	var prevNet, prevDisk counters
	prevTime := time.Now()
	prevNet = readNet()
	prevDisk = readDiskIO()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var f Fast
			f.Time = now
			f.Uptime = now.Sub(st.BootTime)

			if cores, err := cpu.Percent(0, true); err == nil {
				f.CPUCores = cores
				var sum float64
				for _, c := range cores {
					sum += c
				}
				if len(cores) > 0 {
					f.CPUTotal = sum / float64(len(cores))
				}
			}
			if a, err := load.Avg(); err == nil {
				f.Load = [3]float64{a.Load1, a.Load5, a.Load15}
			}
			if v, err := mem.VirtualMemory(); err == nil {
				f.MemTotal, f.MemUsed, f.MemAvail = v.Total, v.Used, v.Available
				// darwin never fills Cached; Wired/Active/Compressed are the
				// numbers Activity Monitor actually shows.
				f.MemWired, f.MemActive = v.Wired, v.Active
				f.MemComp = v.Cached + v.Inactive
			}
			if sw, err := mem.SwapMemory(); err == nil {
				f.SwapTotal, f.SwapUsed = sw.Total, sw.Used
			}

			elapsed := now.Sub(prevTime).Seconds()
			if elapsed <= 0 {
				elapsed = every.Seconds()
			}
			n := readNet()
			d := readDiskIO()
			f.NetRx = rate(n.a, prevNet.a, elapsed)
			f.NetTx = rate(n.b, prevNet.b, elapsed)
			f.NetRxTotal, f.NetTxTotal, f.NetIface = n.a, n.b, n.name
			f.DiskRead = rate(d.a, prevDisk.a, elapsed)
			f.DiskWrite = rate(d.b, prevDisk.b, elapsed)
			f.DiskReadTotal, f.DiskWriteTotal = d.a, d.b
			prevNet, prevDisk, prevTime = n, d, now

			publish(out, f)
		}
	}
}

type counters struct {
	a, b uint64 // rx/read, tx/write
	name string
}

// rate guards against counter resets (interface down, device replaced), which
// would otherwise render as an enormous spike.
func rate(cur, prev uint64, secs float64) float64 {
	if cur < prev || secs <= 0 {
		return 0
	}
	return float64(cur-prev) / secs
}

// readNet sums every physical interface and names the busiest one, so a
// machine on ethernet and wifi at once still reports its real throughput.
func readNet() counters {
	cs, err := net.IOCounters(true)
	if err != nil {
		return counters{}
	}
	var out counters
	var best uint64
	for _, c := range cs {
		if !keepIface(c.Name) {
			continue
		}
		out.a += c.BytesRecv
		out.b += c.BytesSent
		if c.BytesRecv+c.BytesSent > best {
			best, out.name = c.BytesRecv+c.BytesSent, c.Name
		}
	}
	return out
}

// keepIface drops loopback and the virtual interfaces macOS always has up
// (AirDrop, VPN utuns, bridges), which would double-count local traffic.
func keepIface(n string) bool {
	for _, p := range []string{"lo", "awdl", "llw", "utun", "bridge", "gif", "stf", "ap", "anpi", "vmenet"} {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	return true
}

func readDiskIO() counters {
	m, err := disk.IOCounters()
	if err != nil {
		return counters{}
	}
	var out counters
	for _, c := range m {
		out.a += c.ReadBytes
		out.b += c.WriteBytes
	}
	return out
}

func pollSlow(ctx context.Context, every time.Duration, out chan Slow) {
	prev := map[int32]cpuMark{}
	users := map[uint32]string{}
	memTotal := uint64(1)
	if v, err := mem.VirtualMemory(); err == nil && v.Total > 0 {
		memTotal = v.Total
	}

	// Prime once so the first published sample already has real CPU deltas.
	scanProcs(prev, users, memTotal)

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			publish(out, Slow{
				Time:  now,
				Procs: scanProcs(prev, users, memTotal),
				Disks: readDisks(),
				Temps: readTemps(),
			})
		}
	}
}

type cpuMark struct {
	secs float64
	at   time.Time
}

// scanProcs enumerates processes and turns cumulative CPU time into an
// instantaneous percentage by differencing against the previous scan. prev is
// mutated in place and pruned of processes that have exited.
func scanProcs(prev map[int32]cpuMark, users map[uint32]string, memTotal uint64) []Proc {
	ps, err := process.Processes()
	if err != nil {
		return nil
	}
	now := time.Now()
	out := make([]Proc, 0, len(ps))
	seen := make(map[int32]bool, len(ps))

	for _, p := range ps {
		ts, err := p.Times()
		if err != nil {
			continue // gone, or ours to read: skip rather than show a ghost
		}
		seen[p.Pid] = true
		used := ts.User + ts.System

		var pct float64
		if m, ok := prev[p.Pid]; ok {
			if dt := now.Sub(m.at).Seconds(); dt > 0 && used >= m.secs {
				pct = (used - m.secs) / dt * 100
			}
		}
		prev[p.Pid] = cpuMark{secs: used, at: now}

		pr := Proc{PID: p.Pid, CPU: pct}
		pr.Name, _ = p.Name()
		pr.PPID, _ = p.Ppid()
		pr.Threads, _ = p.NumThreads()
		// Root-owned processes deny these to an unprivileged reader; the zero
		// value is the honest answer rather than a reason to drop the row.
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			pr.RSS = mi.RSS
			pr.MemPct = float64(mi.RSS) / float64(memTotal) * 100
		}
		if uids, err := p.Uids(); err == nil && len(uids) > 0 {
			uid := uint32(uids[0])
			un, ok := users[uid]
			if !ok {
				un, _ = p.Username()
				users[uid] = un
			}
			pr.User = un
		}
		if cmd, err := p.Cmdline(); err == nil && cmd != "" {
			pr.Command = cmd
		} else {
			pr.Command = pr.Name
		}
		if pr.Command == "" {
			// Some root-owned processes deny both the name and the argv to an
			// unprivileged reader. Naming them by pid beats a blank row.
			pr.Command = "[pid " + strconv.Itoa(int(p.Pid)) + "]"
		}
		out = append(out, pr)
	}

	for pid := range prev {
		if !seen[pid] {
			delete(prev, pid)
		}
	}
	return out
}

// readDisks lists real filesystems. macOS mounts a dozen synthetic volumes
// (Preboot, VM, xarts, the read-only system snapshot) that all report the
// container's numbers; showing them would be a wall of identical rows.
func readDisks() []Disk {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil
	}
	var out []Disk
	seen := map[string]bool{}
	for _, p := range parts {
		if !keepMount(p.Mountpoint) {
			continue
		}
		u, err := disk.Usage(p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		// One row per device: "/" and "/System/Volumes/Data" are the same store.
		if seen[p.Device] {
			continue
		}
		seen[p.Device] = true
		out = append(out, Disk{
			Mount: p.Mountpoint, Device: p.Device, FSType: p.Fstype,
			Total: u.Total, Used: u.Used, Percent: u.UsedPercent,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mount == "/" != (out[j].Mount == "/") {
			return out[i].Mount == "/"
		}
		return out[i].Total > out[j].Total
	})
	return out
}

func keepMount(m string) bool {
	if m == "/" {
		return true
	}
	if strings.HasPrefix(m, "/Volumes/") {
		return true
	}
	return false
}

// publish delivers the newest sample without ever blocking the poller: if the
// consumer has not drained the previous one, that one is stale and discarded.
func publish[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- v:
		default:
		}
	}
}
