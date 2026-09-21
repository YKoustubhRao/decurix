package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/resourcemon"
)

type mockResourceDesktopManager struct {
	desktops []desktop.Desktop
	updates  []desktop.ResourceAllocation
}

func (m *mockResourceDesktopManager) List(ctx context.Context) ([]desktop.Desktop, error) {
	return m.desktops, nil
}

func (m *mockResourceDesktopManager) Stats(ctx context.Context, id string) (*container.StatsResponse, error) {
	return nil, nil
}

func (m *mockResourceDesktopManager) UpdateResources(ctx context.Context, id string, cpus int64, memoryMB int64) error {
	m.updates = append(m.updates, desktop.ResourceAllocation{CPUs: cpus, MemoryMB: memoryMB})
	for i, d := range m.desktops {
		if d.ID == id {
			m.desktops[i].CPUs = cpus
			m.desktops[i].MemoryMB = memoryMB
		}
	}
	return nil
}

func TestResourceAlertsAndActions(t *testing.T) {
	mockDM := &mockResourceDesktopManager{
		desktops: []desktop.Desktop{
			{
				ID:       "vm-test-1",
				Name:     "Test Desktop",
				Status:   "running",
				CPUs:     2,
				MemoryMB: 2048,
			},
		},
	}

	mon := resourcemon.NewMonitor(mockDM)
	s := &Server{
		ResourceMon: mon,
	}

	// 1. Initially no alerts
	req := httptest.NewRequest(http.MethodGet, "/api/desktops/resource-alerts?id=vm-test-1", nil)
	rr := httptest.NewRecorder()
	s.resourceAlertsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["alert"] != nil {
		t.Fatalf("expected nil alert initially, got %v", res["alert"])
	}

	// 2. Trigger high resource report (CPU 88%, Memory 91%)
	reportJSON := `{"desktop_id":"vm-test-1","cpu_percent":88.0,"memory_percent":91.0}`
	req = httptest.NewRequest(http.MethodPost, "/api/desktops/resource-report", strings.NewReader(reportJSON))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.resourceReportHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on report, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// 3. Check active alert
	req = httptest.NewRequest(http.MethodGet, "/api/desktops/resource-alerts?id=vm-test-1", nil)
	rr = httptest.NewRecorder()
	s.resourceAlertsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var alertResp struct {
		Alert *resourcemon.Alert `json:"alert"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &alertResp); err != nil {
		t.Fatalf("unmarshal alert resp: %v", err)
	}
	if alertResp.Alert == nil {
		t.Fatalf("expected active alert")
	}
	if alertResp.Alert.PredictedCPUs != 4 || alertResp.Alert.PredictedMemoryMB != 4096 {
		t.Fatalf("expected 4 CPUs & 4096 MB, got %d & %d", alertResp.Alert.PredictedCPUs, alertResp.Alert.PredictedMemoryMB)
	}

	// 4. User clicks "Yes, Increase"
	form := url.Values{}
	form.Set("desktop_id", "vm-test-1")
	req = httptest.NewRequest(http.MethodPost, "/api/desktops/resource-increase", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	s.resourceIncreaseHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on increase, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mockDM.updates) == 0 {
		t.Fatalf("expected desktop update to be recorded")
	}
	if mockDM.updates[0].CPUs != 4 || mockDM.updates[0].MemoryMB != 4096 {
		t.Fatalf("expected mock update 4 CPUs and 4096 MB, got %d and %d", mockDM.updates[0].CPUs, mockDM.updates[0].MemoryMB)
	}

	// 5. Verify alert is now cleared
	req = httptest.NewRequest(http.MethodGet, "/api/desktops/resource-alerts?id=vm-test-1", nil)
	rr = httptest.NewRecorder()
	s.resourceAlertsHandler(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &alertResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if alertResp.Alert != nil {
		t.Fatalf("expected alert cleared after increase, got %v", alertResp.Alert)
	}

	// 6. Test Dismiss (User clicks "No")
	// Re-trigger alert on vm-test-2
	mockDM.desktops = append(mockDM.desktops, desktop.Desktop{ID: "vm-test-2", CPUs: 2, MemoryMB: 2048})
	_, _ = mon.RecordReport("vm-test-2", 85.0, 50.0)

	dismissForm := url.Values{}
	dismissForm.Set("desktop_id", "vm-test-2")
	req = httptest.NewRequest(http.MethodPost, "/api/desktops/resource-dismiss", strings.NewReader(dismissForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	s.resourceDismissHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on dismiss, got %d", rr.Code)
	}
	if mon.GetAlert("vm-test-2") != nil {
		t.Fatalf("expected vm-test-2 alert to be dismissed")
	}
}
