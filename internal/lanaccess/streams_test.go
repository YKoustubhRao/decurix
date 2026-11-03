package lanaccess

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// streamRegistry is a device registry with one paired device, "phone", holding token "t".
type streamRegistry struct{}

func (streamRegistry) Pair(string) (string, error)          { return "t", nil }
func (streamRegistry) Valid(token string) bool              { return token == "t" }
func (streamRegistry) Forget(string) error                  { return nil }
func (streamRegistry) DeviceID(token string) (string, bool) { return "phone", token == "t" }

// Unpairing a device ends the screen it has open through the gate: a taken-over connection is closed, and a stream
// that waits on its request's context is ended (RW1-B-04).
func TestUnpairingADeviceEndsWhatItHasOpen(t *testing.T) {
	store := NewStore()
	store.SetDevices(streamRegistry{})
	streamEnded := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 1)
		conn.Read(buf) // held open until something closes it
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(streamEnded)
	})
	srv := httptest.NewServer(store.Middleware(mux))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nCookie: " + deviceCookie + "=t\r\n\r\n"))
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || line[:12] != "HTTP/1.1 101" {
		t.Fatalf("upgrade: %q %v", line, err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: deviceCookie, Value: "t"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	if n := store.CloseDevice("phone"); n != 2 {
		t.Fatalf("closed %d streams, want 2", n)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader.ReadString('\n')
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("the taken-over connection is still open")
	}
	select {
	case <-streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream was not ended")
	}
}
