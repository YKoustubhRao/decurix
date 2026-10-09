// The Windows companion to the Linux control plane's model-selection service.
package main

import (
	"flag"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/joho/godotenv"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:11436", "Windows adapter and selector port")
	base := flag.String("ollama", "http://127.0.0.1:11435", "native Ollama API")
	settings := flag.String("settings", "", "editable .env with model-selection tuning")
	flag.Parse()
	if *settings != "" {
		if err := godotenv.Load(*settings); err != nil {
			log.Fatal(err)
		}
	}
	// Probes in this process must use Windows APIs, never call back into itself
	// or consume a stale resource snapshot from an earlier app version.
	os.Unsetenv("DECURIX_WINDOWS_OLLAMA_URL")
	os.Unsetenv("DECURIX_WINDOWS_SELECTOR_URL")
	server := &http.Server{
		Addr: *listen, Handler: ollama.HostSelectionHandler(*base),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	log.Printf("Windows model selector listening on %s", *listen)
	log.Fatal(server.ListenAndServe())
}
