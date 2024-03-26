package main

import (
	"fmt"
	"github.com/containerd/cgroups/v3/cgroup2"
	"log"
	"os"
	"os/exec"
	"time"
)

func getMaxMemory() int64 {
	return 1 * 1024 * 1024 * 1024
}

func getMaxCPU() uint64 {
	return 50000
}

func monitorMemoryAndCPU(cgroup *cgroup2.Manager, processFinished chan bool) {
	for {
		select {
		// Exit when the process has finished
		case <-processFinished:
			return
		default:
			maxMemoryBytes := getMaxMemory()
			maxCPUPercent := getMaxCPU()

			res := cgroup2.Resources{
				Memory: &cgroup2.Memory{
					Max: &maxMemoryBytes,
				},
				CPU: &cgroup2.CPU{
					Max: cgroup2.NewCPUMax(nil, &maxCPUPercent),
				},
			}
			// update
			if err := cgroup.Update(&res); err != nil {
				return
			}
			time.Sleep(1 * time.Second) // Check every second
		}
	}
}

// Create a cgroup and put the process in it
func createCgroup(proc *exec.Cmd) *cgroup2.Manager {
	res := cgroup2.Resources{}

	// Create a new cgroup
	cgroupName := fmt.Sprintf("process_scaler_%d.slice", proc.Process.Pid)
	m, err := cgroup2.NewSystemd("/", cgroupName, -1, &res)
	if err != nil {
		log.Fatal(err)
	}

	// Add the process to the cgroup
	if err := m.AddProc(uint64(proc.Process.Pid)); err != nil {
		log.Fatal(err)
	}

	return m
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run main.go <command> <args>")
	}

	// Run external program
	proc := exec.Command(os.Args[1], os.Args[2:]...)
	if err := proc.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Process started with PID %d\n", proc.Process.Pid)

	cgroup := createCgroup(proc)

	// Channel to signal when the process has finished
	processFinished := make(chan bool)

	go monitorMemoryAndCPU(cgroup, processFinished)

	// Wait for the program to finish
	if err := proc.Wait(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("Process finished")
	processFinished <- true
	if err := cgroup.DeleteSystemd(); err != nil {
		log.Fatal(err)
	}
}
