package resourcemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

const (
	defaultCPUThreshold  = 80.0
	defaultMemThreshold  = 80.0
	defaultSweepInterval = 5 * time.Second
	defaultCooldown      = 5 * time.Minute
)

// Monitor continuously checks resource usage on managed desktop containers
// and coordinates predictive resource scaling notifications.
type Monitor struct {
	desktops DesktopManager

	CPUThreshold     float64
	MemThreshold     float64
	SweepInterval    time.Duration
	CooldownDuration time.Duration

	mu        sync.Mutex
	alerts    map[string]*Alert
	cooldowns map[string]time.Time

	// now is a test seam, defaulting to time.Now.
	now func() time.Time
}

// NewMonitor creates a new resource monitor backed by the given DesktopManager.
func NewMonitor(desktops DesktopManager) *Monitor {
	cpuThresh := defaultCPUThreshold
	if env := os.Getenv("DECURIX_RESOURCE_CPU_THRESHOLD"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil && v > 0 {
			cpuThresh = v
		}
	}

	memThresh := defaultMemThreshold
	if env := os.Getenv("DECURIX_RESOURCE_MEM_THRESHOLD"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil && v > 0 {
			memThresh = v
		}
	}

	return &Monitor{
		desktops:         desktops,
		CPUThreshold:     cpuThresh,
		MemThreshold:     memThresh,
		SweepInterval:    defaultSweepInterval,
		CooldownDuration: defaultCooldown,
		alerts:           make(map[string]*Alert),
		cooldowns:        make(map[string]time.Time),
		now:              time.Now,
	}
}

// Run sweeps running containers periodically until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.SweepInterval)
	defer ticker.Stop()

	for {
		m.Sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep checks resource usage across all running desktops and raises alerts when exceeded.
func (m *Monitor) Sweep(ctx context.Context) {
	desktops, err := m.desktops.List(ctx)
	if err != nil {
		return
	}

	now := m.now()

	for _, d := range desktops {
		if d.Status != "running" {
			// Clear any leftover alert if container is no longer running
			m.mu.Lock()
			delete(m.alerts, d.ID)
			m.mu.Unlock()
			continue
		}

		m.mu.Lock()
		cooldownUntil, onCooldown := m.cooldowns[d.ID]
		m.mu.Unlock()
		if onCooldown && now.Before(cooldownUntil) {
			continue
		}

		stats, err := m.desktops.Stats(ctx, d.ID)
		if err != nil {
			continue
		}

		cpuPct := CalculateCPUPercent(stats, d.CPUs)
		memPct := CalculateMemoryPercent(stats)

		predCPUs, predMemMB, exceeded, reason, msg := PredictResources(
			d.CPUs, d.MemoryMB,
			cpuPct, memPct,
			m.CPUThreshold, m.MemThreshold,
		)

		m.mu.Lock()
		if exceeded {
			m.alerts[d.ID] = &Alert{
				DesktopID:         d.ID,
				DesktopName:       d.Name,
				CPUPercent:        cpuPct,
				MemoryPercent:     memPct,
				CurrentCPUs:       d.CPUs,
				CurrentMemoryMB:   d.MemoryMB,
				PredictedCPUs:     predCPUs,
				PredictedMemoryMB: predMemMB,
				Message:           msg,
				Reason:            reason,
				TriggeredAt:       now,
			}
		} else {
			// If previously alerted container has calmed down below threshold, remove active alert
			delete(m.alerts, d.ID)
		}
		m.mu.Unlock()
	}
}

// RecordReport evaluates manually or externally reported CPU/Memory usage and registers an alert if exceeded.
func (m *Monitor) RecordReport(desktopID string, cpuPct, memPct float64) (*Alert, error) {
	desktops, err := m.desktops.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list desktops: %w", err)
	}

	var target *desktop.Desktop
	for _, d := range desktops {
		if d.ID == desktopID || desktop.SanitizeID(d.ID) == desktop.SanitizeID(desktopID) {
			target = &d
			break
		}
	}

	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	cooldownUntil, onCooldown := m.cooldowns[desktopID]
	if onCooldown && now.Before(cooldownUntil) {
		return nil, nil // Silently in cooldown
	}

	currentCPUs := int64(2)
	currentMemMB := int64(2048)
	desktopName := desktopID
	if target != nil {
		currentCPUs = target.CPUs
		currentMemMB = target.MemoryMB
		desktopName = target.Name
	}

	predCPUs, predMemMB, exceeded, reason, msg := PredictResources(
		currentCPUs, currentMemMB,
		cpuPct, memPct,
		m.CPUThreshold, m.MemThreshold,
	)

	if !exceeded {
		delete(m.alerts, desktopID)
		return nil, nil
	}

	alert := &Alert{
		DesktopID:         desktopID,
		DesktopName:       desktopName,
		CPUPercent:        cpuPct,
		MemoryPercent:     memPct,
		CurrentCPUs:       currentCPUs,
		CurrentMemoryMB:   currentMemMB,
		PredictedCPUs:     predCPUs,
		PredictedMemoryMB: predMemMB,
		Message:           msg,
		Reason:            reason,
		TriggeredAt:       now,
	}

	m.alerts[desktopID] = alert
	return alert, nil
}

// GetAlert returns the active alert for a desktop ID, or nil if none.
func (m *Monitor) GetAlert(desktopID string) *Alert {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, ok := m.alerts[desktopID]
	if !ok {
		// Try sanitized ID
		sanitized := desktop.SanitizeID(desktopID)
		alert = m.alerts[sanitized]
	}
	if alert == nil {
		return nil
	}

	// Return a copy
	cp := *alert
	return &cp
}

// ListAlerts returns all active alerts.
func (m *Monitor) ListAlerts() []*Alert {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*Alert, 0, len(m.alerts))
	for _, alert := range m.alerts {
		cp := *alert
		out = append(out, &cp)
	}
	return out
}

// Dismiss clears the active alert for the desktop and enters cooldown.
func (m *Monitor) Dismiss(desktopID string) {
	sanitized := desktop.SanitizeID(desktopID)

	m.mu.Lock()
	delete(m.alerts, desktopID)
	delete(m.alerts, sanitized)
	m.cooldowns[desktopID] = m.now().Add(m.CooldownDuration)
	m.cooldowns[sanitized] = m.now().Add(m.CooldownDuration)
	m.mu.Unlock()
}

// Increase applies the predicted (or explicitly requested) resource increase, clears the alert,
// and sets a cooldown.
func (m *Monitor) Increase(
	ctx context.Context,
	desktopID string,
	reqCPUs int64,
	reqMemMB int64,
) (*Alert, error) {
	sanitized := desktop.SanitizeID(desktopID)

	m.mu.Lock()
	alert := m.alerts[desktopID]
	if alert == nil {
		alert = m.alerts[sanitized]
	}

	targetCPUs := reqCPUs
	targetMemMB := reqMemMB

	if alert != nil {
		if targetCPUs <= 0 {
			targetCPUs = alert.PredictedCPUs
		}
		if targetMemMB <= 0 {
			targetMemMB = alert.PredictedMemoryMB
		}
	}
	m.mu.Unlock()

	if targetCPUs <= 0 || targetMemMB <= 0 {
		// Fallback to sensible scaling if no active alert or explicit request was found
		desktops, err := m.desktops.List(ctx)
		if err == nil {
			for _, d := range desktops {
				if d.ID == desktopID || desktop.SanitizeID(d.ID) == sanitized {
					if targetCPUs <= 0 {
						targetCPUs = d.CPUs + 1
					}
					if targetMemMB <= 0 {
						targetMemMB = d.MemoryMB + 1024
					}
					break
				}
			}
		}
	}

	if targetCPUs <= 0 {
		targetCPUs = 2
	}
	if targetMemMB <= 0 {
		targetMemMB = 2048
	}

	if err := m.desktops.UpdateResources(ctx, desktopID, targetCPUs, targetMemMB); err != nil {
		return nil, fmt.Errorf("increase resources: %w", err)
	}

	log.Printf("resourcemon: increased desktop %s to %d CPUs and %d MB RAM", desktopID, targetCPUs, targetMemMB)

	m.mu.Lock()
	delete(m.alerts, desktopID)
	delete(m.alerts, sanitized)
	m.cooldowns[desktopID] = m.now().Add(m.CooldownDuration)
	m.cooldowns[sanitized] = m.now().Add(m.CooldownDuration)
	m.mu.Unlock()

	resultAlert := &Alert{
		DesktopID:         desktopID,
		PredictedCPUs:     targetCPUs,
		PredictedMemoryMB: targetMemMB,
	}

	return resultAlert, nil
}
