package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureRemoteModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		response  string
		wantError string
	}{
		{name: "installed", installed: true},
		{name: "pull", response: "{\"status\":\"downloading\"}\n{\"status\":\"success\"}\n"},
		{name: "API error", response: "{\"error\":\"disk full\"}\n", wantError: "disk full"},
		{name: "truncated download", response: "{\"status\":\"downloading\"}\n", wantError: "EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pulled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/show":
					if !tc.installed {
						w.WriteHeader(http.StatusNotFound)
					}
				case "/api/pull":
					pulled = true
					fmt.Fprint(w, tc.response)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			err := ensureRemoteModel(context.Background(), server.URL, "qwen2.5-coder:3b")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %s", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if pulled == tc.installed {
				t.Fatalf("pulled=%v, installed=%v", pulled, tc.installed)
			}
		})
	}
}

func TestWindowsSelectorDrivesControlPlaneSelection(t *testing.T) {
	calls := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/selection" || r.URL.Query().Get("can_pull") != "1" {
			t.Errorf("selector request: %s", r.URL)
		}
		json.NewEncoder(w).Encode(HostSelection{
			Selection: Selection{Model: Candidate{Name: "qwen2.5-coder:7b", Installed: true}, Fits: true},
			Resources: Resources{RAMTotalGB: 32, RAMFreeGB: 12, Cores: 8, CPUBusy: 0.25, GPUName: "Windows GPU", VRAMTotalGB: 12, VRAMFreeGB: 10},
		})
	}))
	defer host.Close()
	t.Setenv("OLLAMA_URL", host.URL)
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", host.URL)
	t.Setenv("DECURIX_WINDOWS_SELECTOR_URL", host.URL)
	t.Setenv("DECURIX_MODEL_AUTOPULL", "1")
	// Old snapshot variables must not override Windows measurements.
	t.Setenv("DECURIX_OLLAMA_RAM_MB", "512")
	selected, resources, mode, err := evaluate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "auto" || selected.Model.Name != "qwen2.5-coder:7b" || resources.RAMFreeGB != 12 || resources.VRAMFreeGB != 10 || resources.CPUBusy != 0.25 {
		t.Fatalf("selection=%+v resources=%+v mode=%s", selected, resources, mode)
	}
	if calls != 1 {
		t.Fatalf("expected Windows selection once, got %d", calls)
	}
}

func TestWindowsSelectorFailureDoesNotUseLinuxMemory(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer host.Close()
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", "http://192.0.2.1:11435")
	t.Setenv("DECURIX_WINDOWS_SELECTOR_URL", host.URL)
	_, err := ProbeResources(context.Background(), "http://192.0.2.1:11435", true)
	if err == nil {
		t.Fatal("failed Windows probe silently used Linux memory")
	}
}

func TestHostSelectionUsesExistingChooser(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{\"models\":[]}")
	}))
	defer upstream.Close()
	request := httptest.NewRequest(http.MethodGet, "/selection?can_pull=1", nil)
	response := httptest.NewRecorder()
	HostSelectionHandler(upstream.URL).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("host selection: %d %s", response.Code, response.Body.String())
	}
	var host HostSelection
	if err := json.Unmarshal(response.Body.Bytes(), &host); err != nil {
		t.Fatal(err)
	}
	expected := Choose(host.Resources, nil, true, "")
	if host.Selection.Model.Name != expected.Model.Name {
		t.Fatalf("host chooser = %s, existing chooser = %s", host.Selection.Model.Name, expected.Model.Name)
	}
	if host.Resources.RAMTotalGB <= 0 || host.Resources.Cores < 1 {
		t.Fatalf("missing host measurements: %+v", host.Resources)
	}
}

func TestNativeSelectorFailureStopsModelProvisioning(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/selection" {
			t.Errorf("unexpected fallback model request: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer host.Close()
	t.Setenv("OLLAMA_MODEL", "auto")
	t.Setenv("OLLAMA_URL", host.URL)
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", host.URL)
	t.Setenv("DECURIX_WINDOWS_SELECTOR_URL", host.URL)
	t.Setenv("DECURIX_MODEL_AUTOPULL", "1")
	auto.mu.Lock()
	enabled := auto.enabled
	auto.enabled = true
	auto.mu.Unlock()
	defer func() { auto.mu.Lock(); auto.enabled = enabled; auto.mu.Unlock() }()
	err := NewManager(nil, "unused").EnsureReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "select model on inference host") {
		t.Fatalf("expected selection failure before provisioning, got %v", err)
	}
}
