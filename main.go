package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type maxIO struct {
	read  uint64
	write uint64
}

type lsblkOutputListJSON struct {
	Blockdevices []lsblkOutputJSON `json:"blockdevices"`
}

type lsblkOutputJSON struct {
	Name     string            `json:"name"`
	Kname    string            `json:"kname"`
	MajMin   string            `json:"maj:min"`
	Type     string            `json:"type"`
	Children []lsblkOutputJSON `json:"children"`
}

type lastCPUTimeStats struct {
	sync.Mutex
	system []cpu.TimesStat // CPU time for the whole system
	cg     uint64          // CPU time for the cgroup
}

type lastIOCountersStats struct {
	sync.Mutex
	system map[string]disk.IOCountersStat
	cg     []*stats.IOEntry
}

var (
	lastCPUTimes   lastCPUTimeStats
	lastIOCounters lastIOCountersStats
	lsblk          map[string]lsblkOutputJSON
	ioBenchmark    map[string]maxIO // Max read/write in bytes for one second for each device
)

const (
	Margin = 0.1
)

func initCPUTimes(cgManager *cgroup2.Manager) {
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
}

func initIOCounters(cgManager *cgroup2.Manager) {
	lastIOCounters.Lock()

	counters, err := disk.IOCounters()
	if err != nil {
		log.Fatal(err)
	}
	lastIOCounters.system = counters

	cgStats, err := cgManager.Stat()
	if err != nil {
		log.Fatal(err)
	}
	lastIOCounters.cg = cgStats.GetIo().GetUsage()

	lastIOCounters.Unlock()
}

func getMaxMemory(cgStat *stats.MemoryStat) int64 {
	v, err := mem.VirtualMemory()
	if err != nil {
		log.Fatal(err)
	}

	cgMem := int64(cgStat.GetUsageLimit())
	availableMem := float64(v.Available)
	totalMem := float64(v.Total)

	memMargin := totalMem * Margin
	// If available memory less than margin, readjust
	if availableMem < memMargin {
		return cgMem - int64(memMargin-availableMem)
	}
	// If available memory more than margin, readjust
	return cgMem + int64(availableMem-memMargin)
}

// Copied from https://github.com/shirou/gopsutil/blob/v3.24.2/cpu/cpu.go#L104
func getAllBusy(t cpu.TimesStat) (float64, float64) {
	tot := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal

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
	totalCPU := math.Max(0, curAll-lastAll) * 1e6 // Seconds to microseconds
	availableCPU := math.Max(0, totalCPU-math.Max(0, curBusy-lastBusy)*1e6)

	cpuMargin := totalCPU * Margin
	// If available CPU less than margin, readjust
	if availableCPU < cpuMargin {
		return int64(100000 * (cgCPU - (cpuMargin - availableCPU)) / totalCPU), 100000 // 100ms period
	}
	// If available CPU more than margin, readjust
	return int64(100000 * (cgCPU + (availableCPU - cpuMargin)) / totalCPU), 100000
}

func setMaxIO(outputCmd []byte, max *maxIO, read bool) {
	// Get last (unit) and before last (value) word of last line of the output
	words := bytes.Fields(outputCmd)
	value, err := strconv.ParseFloat(string(words[len(words)-2]), 64)
	if err != nil {
		return
	}

	var result uint64
	// ex: MB/sec => MB
	unit := strings.Split(string(words[len(words)-1]), "/")[0]
	switch unit {
	case "kB":
		result = uint64(value * 1024)
	case "MB":
		result = uint64(value * 1024 * 1024)
	case "GB":
		result = uint64(value * 1024 * 1024 * 1024)
	case "TB":
		result = uint64(value * 1024 * 1024 * 1024 * 1024)
	default:
		result = uint64(value)
	}

	if read {
		max.read += result
	} else {
		max.write += result
	}
}

func benchmarkReadIO(device lsblkOutputJSON, max *maxIO) {
	hdparm := exec.Command("sudo", "hdparm", "-Tt", "/dev/"+device.Kname)
	outputHdparmCmd, err := hdparm.Output()
	if err == nil {
		setMaxIO(outputHdparmCmd, max, true)
	}
}

func benchmarkWriteIO(device lsblkOutputJSON, uniqueFileName string, max *maxIO) {
	// Mount the device
	mount := exec.Command("sudo", "mount", "/dev/"+device.Kname, "/tmp")
	if err := mount.Run(); err != nil {
		return
	}

	dd := exec.Command("sudo dd", "if=/dev/zero", "of="+uniqueFileName, "bs=8k", "count=10k")

	var outputDdCmd bytes.Buffer
	dd.Stderr = &outputDdCmd

	if err := dd.Run(); err == nil {
		setMaxIO(outputDdCmd.Bytes(), max, false)
	}

	_ = exec.Command("sudo", "sync", uniqueFileName).Run()
	_ = exec.Command("sudo", "rm", "-f", uniqueFileName).Run()
	_ = exec.Command("sudo", "umount", "/tmp").Run()
}

func recursiveBenchmarkIO(device lsblkOutputJSON, uniqueFileName *string, max *maxIO) {
	if device.Children != nil && len(device.Children) > 0 {
		for _, child := range device.Children {
			recursiveBenchmarkIO(child, uniqueFileName, max)
		}
	}
	benchmarkReadIO(device, max)
	benchmarkWriteIO(device, *uniqueFileName, max)
}

// Benchmark IO speed for each device
// Method: https://askubuntu.com/a/87036
func benchmarkIO() {
	fmt.Println("Before running the process, benchmarking IO...")

	lsblk = make(map[string]lsblkOutputJSON)
	ioBenchmark = make(map[string]maxIO)

	// Run lsblk command to get the list of block devices with their major and minor numbers
	lsblkCmd := exec.Command("sudo", "lsblk", "-anJo", "NAME,KNAME,MAJ:MIN,TYPE")
	outputLsblkCmd, err := lsblkCmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	var lsblkOutput lsblkOutputListJSON
	if err = json.Unmarshal(outputLsblkCmd, &lsblkOutput); err != nil {
		log.Fatal(err)
	}
	// Filter to remove all non-physical devices
	// We don't go deeper than the first level of children
	// Because physical devices are at the first level
	for _, device := range lsblkOutput.Blockdevices {
		if device.Type == "disk" {
			lsblk[device.Kname] = device
		}
	}

	uniqueFileName := fmt.Sprintf("/tmp/output_%s", uuid.New().String())

	for _, device := range lsblk {
		max := maxIO{
			read:  0,
			write: 0,
		}
		recursiveBenchmarkIO(device, &uniqueFileName, &max)
		ioBenchmark[device.Kname] = max
	}

	fmt.Println("Finished benchmarking IO")
}

func findWithMajorMinor(counters []*stats.IOEntry, major, minor uint64) *stats.IOEntry {
	for _, v := range counters {
		if v.Major == major && v.Minor == minor {
			return v
		}
	}
	return nil
}

func getMaxIO(cgStat *stats.IOStat) []cgroup2.Entry {
	curCgCounters := cgStat.GetUsage()

	curCounters, err := disk.IOCounters()
	if err != nil {
		log.Fatal(err)
	}

	// Mutex lock
	lastIOCounters.Lock()
	defer lastIOCounters.Unlock()

	lastCgCounters := lastIOCounters.cg
	lastIOCounters.cg = curCgCounters

	lastCounters := lastIOCounters.system
	lastIOCounters.system = curCounters

	result := make([]cgroup2.Entry, 0)

	for deviceName, curCounter := range curCounters {
		device, exists := lsblk[deviceName]
		if !exists {
			continue
		}

		var major, minor int64
		if _, err = fmt.Sscanf(device.MajMin, "%d:%d", &major, &minor); err != nil {
			continue
		}

		lastCounter := lastCounters[deviceName]
		curCgCounter := findWithMajorMinor(curCgCounters, uint64(major), uint64(minor))
		lastCgCounter := findWithMajorMinor(lastCgCounters, uint64(major), uint64(minor))

		if (lastCounter != disk.IOCountersStat{}) {
			// Read
			cgBytesRead := math.Max(0, float64(curCgCounter.GetRbytes()-lastCgCounter.GetRbytes()))
			maxBytesRead := float64(ioBenchmark[deviceName].read)
			availableBytesRead := math.Max(0, maxBytesRead-math.Max(0, float64(curCounter.ReadBytes-lastCounter.ReadBytes)))

			readMargin := maxBytesRead * Margin

			readEntry := cgroup2.Entry{
				Type:  cgroup2.ReadBPS,
				Major: major,
				Minor: minor,
			}
			// If available IO read less than margin, readjust
			if availableBytesRead < readMargin {
				readEntry.Rate = uint64(cgBytesRead - (readMargin - availableBytesRead))
			} else {
				readEntry.Rate = uint64(cgBytesRead + (availableBytesRead - readMargin))
			}
			if readEntry.Rate > 0 {
				result = append(result, readEntry)
			}

			// Write
			cgBytesWrite := math.Max(0, float64(curCgCounter.GetWbytes()-lastCgCounter.GetWbytes()))
			maxBytesWrite := float64(ioBenchmark[deviceName].write)
			availableBytesWrite := math.Max(0, maxBytesWrite-math.Max(0, float64(curCounter.WriteBytes-lastCounter.WriteBytes)))

			writeMargin := maxBytesWrite * Margin

			writeEntry := cgroup2.Entry{
				Type:  cgroup2.WriteBPS,
				Major: major,
				Minor: minor,
			}
			// If available IO write less than margin, readjust
			if availableBytesWrite < writeMargin {
				writeEntry.Rate = uint64(cgBytesWrite - (writeMargin - availableBytesWrite))
			} else {
				writeEntry.Rate = uint64(cgBytesWrite + (availableBytesWrite - writeMargin))
			}
			if writeEntry.Rate > 0 {
				result = append(result, writeEntry)
			}
		}
	}

	return result
}

func monitorResources(cgManager *cgroup2.Manager, processFinished chan bool) {
	fmt.Println("Monitoring resources usage while the process is running")
	initCPUTimes(cgManager)
	initIOCounters(cgManager)
	time.Sleep(1 * time.Second)

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
			maxIOEntry := getMaxIO(cgStats.GetIo())

			res := cgroup2.Resources{
				Memory: &cgroup2.Memory{
					Max: &maxMemoryBytes,
				},
				CPU: &cgroup2.CPU{
					// Runs cpuQuota microseconds every cpuPeriod microseconds
					Max: cgroup2.NewCPUMax(&cpuQuota, &cpuPeriod),
				},
				IO: &cgroup2.IO{
					Max: maxIOEntry,
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

	// Enable the relevant controllers
	if err = m.ToggleControllers([]string{"memory", "cpu", "io"}, cgroup2.Enable); err != nil {
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

	benchmarkIO()

	// Run external program
	proc := exec.Command(os.Args[1], os.Args[2:]...)
	if err := proc.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Process started with PID %d\n", proc.Process.Pid)

	cgManager := createCgroup(proc)

	// Channel to signal when the process has finished
	processFinished := make(chan bool)

	go monitorResources(cgManager, processFinished)

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
