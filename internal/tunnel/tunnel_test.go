package tunnel

import "testing"

func TestStoreRecordsAllThreeAddressesIndependently(t *testing.T) {
	s := NewStore()

	if got := s.Get(); got != "" {
		t.Fatalf("Get() on a fresh store = %q, want empty", got)
	}
	if got := s.AdminURL(); got != "" {
		t.Fatalf("AdminURL() on a fresh store = %q, want empty", got)
	}
	if got := s.LANURL(); got != "" {
		t.Fatalf("LANURL() on a fresh store = %q, want empty", got)
	}

	s.Set("https://foo.trycloudflare.com")
	s.SetAdminURL("http://192.168.1.42:8080")
	s.SetLANURL("http://192.168.1.42:8090")

	if got := s.Get(); got != "https://foo.trycloudflare.com" {
		t.Fatalf("Get() = %q", got)
	}
	if got := s.AdminURL(); got != "http://192.168.1.42:8080" {
		t.Fatalf("AdminURL() = %q", got)
	}
	if got := s.LANURL(); got != "http://192.168.1.42:8090" {
		t.Fatalf("LANURL() = %q", got)
	}
}
