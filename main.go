package main

import (
	"fmt"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/shirou/gopsutil/v3/mem"
	"log"
	"os"
	"os/exec"
	"time"
)

const (
	Margin = 0.1
)

func getMaxMemory(cgStat *stats.MemoryStat) int64 {
	v, err := mem.VirtualMemory()
	if err != nil {
		log.Fatal(err)
	}

	cgMem := float64(cgStat.GetUsageLimit())
	availableMem := float64(v.Available)
	totalMem := float64(v.Total)

	// If available memory less than 10% of total memory, readjust to get a margin of 10%
	if availableMem < totalMem*Margin {
		return int64((cgMem - availableMem) * (1 - Margin))
	}
	// If available memory more than 10% of total memory, readjust to get a margin of 10%
	return int64((cgMem + availableMem) * (1 - Margin))
}

func getMaxCPU(cgStat *stats.CPUStat) (int64, uint64) {
	return 50000, 100000 // runs for 50ms every 100ms, so 50% CPU
}

func monitorMemoryAndCPU(cgManager *cgroup2.Manager, processFinished chan bool) {
	fmt.Println("Monitoring memory and CPU usage while the process is running")
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
