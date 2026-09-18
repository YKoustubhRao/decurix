package resourcemon

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

type mockDesktopManager struct {
	desktops []desktop.Desktop
	stats    map[string]*container.StatsResponse
	updates  []desktop.ResourceAllocation
}

func (m *mockDesktopManager) List(ctx context.Context) ([]desktop.Desktop, error) {
	return m.desktops, nil
}

func (m *mockDesktopManager) Stats(ctx context.Context, id string) (*container.StatsResponse, error) {
	return m.stats[id], nil
}

func (m *mockDesktopManager) UpdateResources(ctx context.Context, id string, cpus int64, memoryMB int64) error {
	m.updates = append(m.updates, desktop.ResourceAllocation{CPUs: cpus, MemoryMB: memoryMB})
	for i, d := range m.desktops {
		if d.ID == id {
			m.desktops[i].CPUs = cpus
			m.desktops[i].MemoryMB = memoryMB
		}
	}
	return nil
}

func TestPredictResources(t *testing.T) {
	t.Run("below thresholds produces no prediction", func(t *testing.T) {
		predCPUs, predMem, exceeded, _, _ := PredictResources(2, 2048, 50.0, 60.0, 80.0, 80.0)
		if exceeded {
			t.Errorf("expected exceeded to be false, got true")
		}
		if predCPUs != 2 || predMem != 2048 {
			t.Errorf("expected 2 CPUs and 2048 MB, got %d CPUs and %d MB", predCPUs, predMem)
		}
	})

	t.Run("high CPU only predicts increased CPU", func(t *testing.T) {
		predCPUs, predMem, exceeded, reason, msg := PredictResources(2, 2048, 88.0, 60.0, 80.0, 80.0)
		if !exceeded {
			t.Fatalf("expected exceeded to be true")
		}
		if predCPUs != 4 {
			t.Errorf("expected CPU increase to 4, got %d", predCPUs)
		}
		if predMem != 2048 {
			t.Errorf("expected memory to remain 2048, got %d", predMem)
		}
		if reason == "" || msg == "" {
			t.Errorf("expected non-empty reason and message")
		}
	})

	t.Run("high memory only predicts increased RAM", func(t *testing.T) {
		predCPUs, predMem, exceeded, _, _ := PredictResources(2, 2048, 40.0, 85.0, 80.0, 80.0)
		if !exceeded {
			t.Fatalf("expected exceeded to be true")
		}
		if predCPUs != 2 {
			t.Errorf("expected CPU to remain 2, got %d", predCPUs)
		}
		if predMem != 4096 {
			t.Errorf("expected memory to increase to 4096, got %d", predMem)
		}
	})

	t.Run("both high predicts increases for both", func(t *testing.T) {
		predCPUs, predMem, exceeded, _, _ := PredictResources(2, 2048, 92.0, 89.0, 80.0, 80.0)
		if !exceeded {
			t.Fatalf("expected exceeded to be true")
		}
		if predCPUs != 4 {
			t.Errorf("expected 4 CPUs, got %d", predCPUs)
		}
		if predMem != 4096 {
			t.Errorf("expected 4096 MB, got %d", predMem)
		}
	})
}

func TestMonitorSweepAndAlertLifecycle(t *testing.T) {
	mockDM := &mockDesktopManager{
		desktops: []desktop.Desktop{
			{
				ID:       "vm-1",
				Name:     "Test VM",
				Status:   "running",
				CPUs:     2,
				MemoryMB: 2048,
			},
		},
		stats: map[string]*container.StatsResponse{
			"vm-1": {
				CPUStats: container.CPUStats{
					CPUUsage:    container.CPUUsage{TotalUsage: 200000000},
					SystemUsage: 100000000,
					OnlineCPUs:  1,
				},
				PreCPUStats: container.CPUStats{
					CPUUsage:    container.CPUUsage{TotalUsage: 100000000},
					SystemUsage: 50000000,
				},
				MemoryStats: container.MemoryStats{
					Usage: 1800 * 1024 * 1024,
					Limit: 2048 * 1024 * 1024,
				},
			},
		},
	}

	mon := NewMonitor(mockDM)
	fixedTime := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	mon.now = func() time.Time { return fixedTime }

	// Run sweep
	mon.Sweep(context.Background())

	alert := mon.GetAlert("vm-1")
	if alert == nil {
		t.Fatalf("expected alert for vm-1 to be present")
	}

	if alert.PredictedMemoryMB != 4096 {
		t.Errorf("expected predicted memory 4096, got %d", alert.PredictedMemoryMB)
	}

	// Test Dismiss sets cooldown
	mon.Dismiss("vm-1")
	if mon.GetAlert("vm-1") != nil {
		t.Fatalf("expected alert to be dismissed")
	}

	// Sweeping again while in cooldown should not re-trigger alert
	mon.Sweep(context.Background())
	if mon.GetAlert("vm-1") != nil {
		t.Fatalf("expected alert to remain suppressed during cooldown")
	}

	// Advance time past cooldown
	fixedTime = fixedTime.Add(6 * time.Minute)
	mon.Sweep(context.Background())
	alert = mon.GetAlert("vm-1")
	if alert == nil {
		t.Fatalf("expected alert to re-trigger after cooldown expiry")
	}

	// Test Increase (user says Yes)
	increased, err := mon.Increase(context.Background(), "vm-1", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error on increase: %v", err)
	}
	if increased.PredictedMemoryMB != 4096 {
		t.Errorf("expected updated memory 4096, got %d", increased.PredictedMemoryMB)
	}
	if len(mockDM.updates) == 0 {
		t.Fatalf("expected UpdateResources to be called on mockDM")
	}
	lastUpdate := mockDM.updates[len(mockDM.updates)-1]
	if lastUpdate.MemoryMB != 4096 {
		t.Errorf("expected desktop memory updated to 4096, got %d", lastUpdate.MemoryMB)
	}

	// Verify alert is cleared and cooldown active
	if mon.GetAlert("vm-1") != nil {
		t.Fatalf("expected alert cleared after increase")
	}
}

func TestRecordReport(t *testing.T) {
	mockDM := &mockDesktopManager{
		desktops: []desktop.Desktop{
			{
				ID:       "vm-2",
				Name:     "Report VM",
				Status:   "running",
				CPUs:     2,
				MemoryMB: 2048,
			},
		},
	}

	mon := NewMonitor(mockDM)
	alert, err := mon.RecordReport("vm-2", 85.0, 90.0)
	if err != nil {
		t.Fatalf("unexpected error from RecordReport: %v", err)
	}
	if alert == nil {
		t.Fatalf("expected active alert")
	}
	if alert.PredictedCPUs != 4 || alert.PredictedMemoryMB != 4096 {
		t.Errorf("expected 4 CPUs and 4096 MB, got %d CPUs and %d MB", alert.PredictedCPUs, alert.PredictedMemoryMB)
	}

	// Dismiss and check
	mon.Dismiss("vm-2")
	if mon.GetAlert("vm-2") != nil {
		t.Errorf("expected alert dismissed")
	}
}
