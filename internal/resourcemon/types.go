package resourcemon

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// DesktopManager defines what the resource monitor needs from desktop.Manager.
type DesktopManager interface {
	List(ctx context.Context) ([]desktop.Desktop, error)
	Stats(ctx context.Context, id string) (*container.StatsResponse, error)
	UpdateResources(ctx context.Context, id string, cpus int64, memoryMB int64) error
}

// Alert represents an active resource warning and recommended increase.
type Alert struct {
	DesktopID         string    `json:"desktop_id"`
	DesktopName       string    `json:"desktop_name"`
	CPUPercent        float64   `json:"cpu_percent"`
	MemoryPercent     float64   `json:"memory_percent"`
	CurrentCPUs       int64     `json:"current_cpus"`
	CurrentMemoryMB   int64     `json:"current_memory_mb"`
	PredictedCPUs     int64     `json:"predicted_cpus"`
	PredictedMemoryMB int64     `json:"predicted_memory_mb"`
	Message           string    `json:"message"`
	Reason            string    `json:"reason"`
	TriggeredAt       time.Time `json:"triggered_at"`
}

// PredictResources computes the recommended CPU and Memory based on usage percentages.
func PredictResources(
	currentCPUs, currentMemMB int64,
	cpuPct, memPct float64,
	cpuThreshold, memThreshold float64,
) (predCPUs, predMemMB int64, exceeded bool, reason string, msg string) {
	if currentCPUs <= 0 {
		currentCPUs = 1
	}
	if currentMemMB <= 0 {
		currentMemMB = 2048
	}

	predCPUs = currentCPUs
	predMemMB = currentMemMB

	cpuExceeded := cpuPct >= cpuThreshold
	memExceeded := memPct >= memThreshold

	if !cpuExceeded && !memExceeded {
		return predCPUs, predMemMB, false, "", ""
	}

	// Predict CPU increase if CPU threshold is met/exceeded
	if cpuExceeded {
		if currentCPUs <= 1 {
			predCPUs = 2
		} else if currentCPUs == 2 {
			predCPUs = 4
		} else {
			predCPUs = currentCPUs + 2
		}
		// Cap CPU at a reasonable max (at least 8, or host NumCPU)
		maxCPU := int64(runtime.NumCPU())
		if maxCPU < 8 {
			maxCPU = 8
		}
		if predCPUs > maxCPU {
			predCPUs = maxCPU
		}
	}

	// Predict Memory increase if Memory threshold is met/exceeded
	if memExceeded {
		if currentMemMB <= 1024 {
			predMemMB = 2048
		} else if currentMemMB <= 2048 {
			predMemMB = 4096
		} else {
			predMemMB = currentMemMB + 2048
		}
	}

	switch {
	case cpuExceeded && memExceeded:
		reason = fmt.Sprintf("High CPU (%.0f%%) and Memory (%.0f%%) usage", cpuPct, memPct)
		msg = fmt.Sprintf("High CPU (%.0f%%) and RAM (%.0f%%) detected. Automatically increase to %d CPUs and %d MB RAM?", cpuPct, memPct, predCPUs, predMemMB)
	case cpuExceeded:
		reason = fmt.Sprintf("High CPU usage (%.0f%%)", cpuPct)
		msg = fmt.Sprintf("High CPU usage (%.0f%%) detected. Automatically increase from %d to %d CPUs?", cpuPct, currentCPUs, predCPUs)
	case memExceeded:
		reason = fmt.Sprintf("High Memory usage (%.0f%%)", memPct)
		msg = fmt.Sprintf("High RAM usage (%.0f%%) detected. Automatically increase from %d MB to %d MB RAM?", memPct, currentMemMB, predMemMB)
	}

	return predCPUs, predMemMB, true, reason, msg
}

// CalculateCPUPercent computes the CPU usage as a percentage of the container's allocated CPUs.
func CalculateCPUPercent(stats *container.StatsResponse, allocatedCPUs int64) float64 {
	if stats == nil {
		return 0
	}
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage) - float64(stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage) - float64(stats.PreCPUStats.SystemUsage)
	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		rawPercent := (cpuDelta / systemDelta) * onlineCPUs * 100.0
		if allocatedCPUs > 0 {
			return rawPercent / float64(allocatedCPUs)
		}
		return rawPercent
	}
	return 0
}

// CalculateMemoryPercent computes memory usage as a percentage of the container's memory limit.
func CalculateMemoryPercent(stats *container.StatsResponse) float64 {
	if stats == nil || stats.MemoryStats.Limit == 0 {
		return 0
	}
	usage := stats.MemoryStats.Usage
	var cache uint64
	if v, ok := stats.MemoryStats.Stats["inactive_file"]; ok {
		cache = v
	} else if v, ok := stats.MemoryStats.Stats["total_inactive_file"]; ok {
		cache = v
	} else if v, ok := stats.MemoryStats.Stats["cache"]; ok {
		cache = v
	}
	if usage > cache {
		usage -= cache
	}
	return (float64(usage) / float64(stats.MemoryStats.Limit)) * 100.0
}
