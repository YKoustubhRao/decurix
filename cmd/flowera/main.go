package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/client"
	"github.com/joho/godotenv"

	"github.com/YKoustubhRao/flowera/internal/claudeauth"
	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/httpapi"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/ollama"
	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
	"github.com/YKoustubhRao/flowera/internal/stepimages"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/tunnel"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

const listenAddr = ":8080"

// version is stamped at build time by scripts/release/release.sh via
// -ldflags "-X main.version=...". It stays "dev" for plain `go build`/`go run`.
var version = "dev"

// loadEnv loads the .env file into the process environment. It looks in the
// working directory first, then next to the executable, so a standalone
// binary dropped on another machine finds the .env shipped beside it.
func loadEnv() {
	if err := godotenv.Load(); err == nil {
		return
	}

	exe, err := os.Executable()
	if err == nil {
		beside := filepath.Join(filepath.Dir(exe), ".env")

		if err := godotenv.Load(beside); err == nil {
			log.Printf("Loaded %s", beside)
			return
		}
	}

	log.Println("No .env file found; using environment variables")
}

// exeName returns the name this binary was invoked as, for use in help text.
func exeName() string {
	exe, err := os.Executable()
	if err != nil {
		return "flowera"
	}

	return filepath.Base(exe)
}

// must stops startup when a store cannot open its tables: a control plane
// that silently kept its records in nowhere would lose them on the next
// restart, which is exactly what the database is for.
func must[T any](store T, err error) T {
	if err != nil {
		log.Fatal(err)
	}

	return store
}

// stepImagesMaxBytes reads FLOWERA_STEP_IMAGES_MB, the disk budget for action
// screenshots; unset or invalid means stepimages.DefaultMaxBytes.
func stepImagesMaxBytes() int {
	megabytes, err := strconv.Atoi(os.Getenv("FLOWERA_STEP_IMAGES_MB"))
	if err != nil || megabytes <= 0 {
		return stepimages.DefaultMaxBytes
	}

	return megabytes << 20
}

// main loads configuration, sets up the Docker client, ensures required images/networks exist, registers HTTP routes, and starts the server.
func main() {
	showVersion := flag.Bool("version", false, "print the Flowera version and exit")
	buildImages := flag.Bool("build-images", false, "build the desktop images from the build context embedded in this binary, then exit")
	rebuild := flag.Bool("rebuild", false, "with -build-images, rebuild images that already exist instead of skipping them")
	flag.Parse()

	if *showVersion {
		fmt.Printf("flowera %s\n", version)
		return
	}

	log.Printf("Flowera %s starting", version)

	loadEnv()

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()

	desktops := desktop.NewManager(cli)
	ollamaManager := ollama.NewManager(cli, desktop.NetworkName)

	// Building all three images takes a long time and a lot of disk, so it is
	// an explicit request rather than something startup does on its own.
	if *buildImages {
		if err := desktops.BuildImages(ctx, os.Stdout, !*rebuild); err != nil {
			log.Fatal(err)
		}

		log.Println("Desktop images ready.")

		return
	}

	// Non-blocking: the dashboard is still useful (and says what is wrong)
	// when the images have not been built yet.
	if err := desktops.EnsureImages(ctx); err != nil {
		log.Printf("warning: %v", err)
		log.Printf("run `%s --build-images` to build them", exeName())
	}

	if err := desktops.EnsureNetwork(ctx); err != nil {
		log.Fatal(err)
	}

	dbPath := filepath.Join(sqlitedb.DataDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	log.Printf("Records: %s", dbPath)

	server := httpapi.NewServer(
		desktops,
		must(helpnotes.NewStore(db)),
		must(memory.NewStore(db)),
		must(conversation.NewStore(db)),
		must(stepimages.NewStore(db, stepImagesMaxBytes())),
		must(tasks.NewStore(db)),
		tunnel.NewStore(),
		must(claudeauth.NewStore(db)),
		webui.NewRenderer(),
	)

	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	// IMPORTANT:
	// Ollama initialization must not block the HTTP server.
	go func() {
		if err := ollamaManager.EnsureReady(context.Background()); err != nil {
			log.Printf("warning: Ollama initialization failed: %v", err)
			return
		}

		log.Println("Ollama ready.")
	}()

	log.Printf("Desktop manager listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
