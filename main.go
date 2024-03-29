package main

import (
	"fmt"
	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"log"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

type lastCPUTimeStats struct {
	sync.Mutex
	system []cpu.TimesStat // CPU time for the whole system
	cg     uint64          // CPU time for the cgroup
}

var (
	lastCPUTimes lastCPUTimeStats
)

const (
	Margin = 0.1
)

func initTimes(cgManager *cgroup2.Manager) {
	lastCPUTimes.Lock()

	times, err := cpu.Times(false)
	if err != nil {
		log.Fatal(err)
	}
	lastCPUTimes.system = times

	cgStats, err := cgManager.Stat()
	if err != nil {
		log.Fatal(err)
	}
	lastCPUTimes.cg = cgStats.GetCPU().GetUsageUsec()

	lastCPUTimes.Unlock()

	time.Sleep(1 * time.Second)
}

func getMaxMemory(cgStat *stats.MemoryStat) int64 {
	v, err := mem.VirtualMemory()
	if err != nil {
		log.Fatal(err)
	}

	cgMem := float64(cgStat.GetUsageLimit())
	availableMem := float64(v.Available)
	totalMem := float64(v.Total)

	// If available memory less than margin, readjust
	if availableMem < totalMem*Margin {
		return int64((cgMem - availableMem) * (1 - Margin))
	}
	// If available memory more than margin, readjust
	return int64((cgMem + availableMem) * (1 - Margin))
}

// Copied from https://github.com/shirou/gopsutil/blob/v3.24.2/cpu/cpu.go#L104
func getAllBusy(t cpu.TimesStat) (float64, float64) {
	tot := t.Total()
	if runtime.GOOS == "linux" {
		tot -= t.Guest     // Linux 2.6.24+
		tot -= t.GuestNice // Linux 3.2.0+
	}

	busy := tot - t.Idle - t.Iowait

	return tot, busy
}

func getMaxCPU(cgStat *stats.CPUStat) (int64, uint64) {
	curCgTimes := cgStat.GetUsageUsec()

	curTimes, err := cpu.Times(false)
	if err != nil {
		log.Fatal(err)
	}

	// Mutex lock
	lastCPUTimes.Lock()
	defer lastCPUTimes.Unlock()

	lastCgTimes := lastCPUTimes.cg
	lastCPUTimes.cg = curCgTimes

	lastTimes := lastCPUTimes.system
	lastCPUTimes.system = curTimes
	if len(lastTimes) == 0 || len(lastTimes) != len(curTimes) {
		log.Fatal("Error: could not get CPU times")
	}
	curAll, curBusy := getAllBusy(curTimes[0])
	lastAll, lastBusy := getAllBusy(lastTimes[0])

	cgCPU := math.Max(0, float64(curCgTimes-lastCgTimes))
	availableCPU := math.Max(0, curBusy-lastBusy) * 1e6 // Seconds to microseconds
	totalCPU := math.Max(0, curAll-lastAll) * 1e6

	cpuMargin := totalCPU * Margin
	// If available CPU less than margin, readjust
	if availableCPU < cpuMargin {
		return int64(100000 * (cgCPU - (cpuMargin - availableCPU)) / totalCPU), 100000 // 100ms period
	}
	// If available CPU more than margin, readjust
	return int64(100000 * (cgCPU + (availableCPU - cpuMargin)) / totalCPU), 100000
}

func monitorMemoryAndCPU(cgManager *cgroup2.Manager, processFinished chan bool) {
	fmt.Println("Monitoring memory and CPU usage while the process is running")
	initTimes(cgManager)
	for {
		select {
		// Exit when the process has finished
		case <-processFinished:
			return
		default:
			cgStats, err := cgManager.Stat()
			if err != nil {
				log.Fatal(err)
			}

			maxMemoryBytes := getMaxMemory(cgStats.GetMemory())
			cpuQuota, cpuPeriod := getMaxCPU(cgStats.GetCPU())

			res := cgroup2.Resources{
				Memory: &cgroup2.Memory{
					Max: &maxMemoryBytes,
				},
				CPU: &cgroup2.CPU{
					// Runs cpuQuota microseconds every cpuPeriod microseconds
					Max: cgroup2.NewCPUMax(&cpuQuota, &cpuPeriod),
				},
			}
			// Update
			if err = cgManager.Update(&res); err != nil {
				log.Fatal(err)
			}
			time.Sleep(1 * time.Second) // Monitor every second
		}
	}
}

// Create a cgroup and put the process in it
func createCgroup(proc *exec.Cmd) *cgroup2.Manager {
	res := cgroup2.Resources{}

	// Create a new cgroup
	cgName := fmt.Sprintf("process_scaler_%d.slice", proc.Process.Pid)
	m, err := cgroup2.NewSystemd("/", cgName, -1, &res)
	if err != nil {
		log.Fatal(err)
	}

	// Enable the memory and CPU controllers
	if err = m.ToggleControllers([]string{"memory", "cpu"}, cgroup2.Enable); err != nil {
		log.Fatal(err)
	}

	// Add the process to the cgroup
	if err = m.AddProc(uint64(proc.Process.Pid)); err != nil {
		log.Fatal(err)
	}

	return m
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run main.go <command> <args>")
	}
	if cgroups.Mode() != cgroups.Unified {
		log.Fatal("This program requires cgroup v2")
	}

	// Run external program
	proc := exec.Command(os.Args[1], os.Args[2:]...)
	if err := proc.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Process started with PID %d\n", proc.Process.Pid)

	cgManager := createCgroup(proc)

	// Channel to signal when the process has finished
	processFinished := make(chan bool)

	go monitorMemoryAndCPU(cgManager, processFinished)

	// Wait for the program to finish
	if err := proc.Wait(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("Process finished")
	processFinished <- true
	if err := cgManager.DeleteSystemd(); err != nil {
		log.Fatal(err)
	}
}
