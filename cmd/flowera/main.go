package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/docker/docker/client"
	"github.com/joho/godotenv"

	"github.com/YKoustubhRao/flowera/internal/claudeauth"
	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/dag"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/httpapi"
	"github.com/YKoustubhRao/flowera/internal/idlepause"
	"github.com/YKoustubhRao/flowera/internal/lanaccess"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/ollama"
	"github.com/YKoustubhRao/flowera/internal/querynotes"
	"github.com/YKoustubhRao/flowera/internal/schedule"
	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
	"github.com/YKoustubhRao/flowera/internal/stepimages"
	"github.com/YKoustubhRao/flowera/internal/suggest"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/tunnel"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

const (
	// listenAddr is the primary listener: unauthenticated, same as always.
	// Desktop agents call back into it (CONTROL_PLANE_URL) and a human at the
	// control plane's own machine uses it directly - see internal/lanaccess's
	// package doc for why it stays exactly as it was.
	listenAddr = ":8080"

	// defaultLANAddr is the second listener, gated by internal/lanaccess -
	// the one a Cloudflare tunnel or a direct LAN address should point at.
	// Overridable with FLOWERA_LAN_PORT.
	defaultLANAddr = ":8090"

	// defaultIdleStopMinutes is internal/idlepause's threshold when
	// FLOWERA_IDLE_STOP_MINUTES is unset - idle-pause is on by default.
	// FLOWERA_IDLE_STOP_MINUTES=0 opts out.
	defaultIdleStopMinutes = 5
)

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

// lanListenAddr reads FLOWERA_LAN_PORT, the port internal/lanaccess's gated
// listener binds; unset or invalid means defaultLANAddr.
func lanListenAddr() string {
	port := os.Getenv("FLOWERA_LAN_PORT")
	if port == "" {
		return defaultLANAddr
	}

	if _, err := strconv.Atoi(port); err != nil {
		log.Printf("warning: FLOWERA_LAN_PORT=%q is not a number; using %s", port, defaultLANAddr)
		return defaultLANAddr
	}

	return ":" + port
}

// idleStopThreshold reads FLOWERA_IDLE_STOP_MINUTES, in minutes.
// internal/idlepause is on by default: unset or invalid means
// defaultIdleStopMinutes, not disabled. FLOWERA_IDLE_STOP_MINUTES=0 (or any
// non-positive value) is the explicit opt-out - Checker.Run treats a
// non-positive Threshold as "do nothing" rather than this function needing
// its own enabled/disabled flag.
func idleStopThreshold() time.Duration {
	raw := os.Getenv("FLOWERA_IDLE_STOP_MINUTES")
	if raw == "" {
		return defaultIdleStopMinutes * time.Minute
	}

	minutes, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("warning: FLOWERA_IDLE_STOP_MINUTES=%q is not a number; using the default of %d minutes", raw, defaultIdleStopMinutes)
		return defaultIdleStopMinutes * time.Minute
	}

	if minutes <= 0 {
		return 0
	}

	return time.Duration(minutes) * time.Minute
}

// main loads configuration, sets up the Docker client, ensures required images/networks exist, registers HTTP routes, and starts the server.
func main() {
	showVersion := flag.Bool("version", false, "print the Flowera version and exit")
	buildImages := flag.Bool("build-images", false, "build the desktop images from the build context embedded in this binary, then exit")
	rebuild := flag.Bool("rebuild", false, "with -build-images, rebuild images that already exist instead of skipping them")
	cliMode := flag.Bool("cli", false, "run an interactive CLI against a running control plane instead of starting the server")
	cliURL := flag.String("url", "http://127.0.0.1:8080", "with -cli, the control plane's URL")
	flag.Parse()

	if *showVersion {
		fmt.Printf("flowera %s\n", version)
		return
	}

	// -cli needs none of the setup below: no Docker client, no database -
	// it is a thin client for a control plane already running somewhere
	// else, reached over the same HTTP API the dashboard uses.
	if *cliMode {
		os.Exit(runCLI(*cliURL))
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
	desktop.OptimizeHostKSM()
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

	// Pre-warm the shared tools volume asynchronously in the background so the
	// first VM creation does not have to wait for volume initialization.
	go func() {
		if err := desktops.EnsureToolsVolume(context.Background()); err != nil {
			log.Printf("background tools volume: %v", err)
		}
	}()

	dbPath := filepath.Join(sqlitedb.DataDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	log.Printf("Records: %s", dbPath)

	tasksStore := must(tasks.NewStore(db))
	lanAccess := lanaccess.NewStore()
	schedules := must(schedule.NewStore(db))
	dags := must(dag.NewStore(db))
	memoryStore := must(memory.NewStore(db))
	suggestions := must(suggest.NewStore(db, desktops, tasksStore, memoryStore, ollama.Embed))

	server := httpapi.NewServer(
		desktops,
		must(querynotes.NewStore(db)),
		memoryStore,
		must(conversation.NewStore(db)),
		must(stepimages.NewStore(db, stepImagesMaxBytes())),
		tasksStore,
		tunnel.NewStore(),
		must(claudeauth.NewStore(db)),
		lanAccess,
		schedules,
		dags,
		suggestions,
		webui.NewRenderer(),
	)

	// internal/dag's Runner needs Server itself as its Submitter
	// (SubmitTracked, which - unlike Submit - hands back the task ID a node's
	// ticket landed under), so it is built after Server and wired back onto
	// it rather than passed through NewServer.
	dagRunner := &dag.Runner{Store: dags, Submitter: server, Tasks: tasksStore}
	server.DagRunner = dagRunner
	go dagRunner.Run(context.Background())

	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	server.RegisterLocalRoutes(mux)

	// The second listener is what a Cloudflare tunnel or a direct LAN
	// address should be pointed at: every route the primary listener serves,
	// gated behind lanAccess.Middleware. It always starts - the access code
	// gating it is what makes that safe - so build.sh/rebuild.sh can always
	// point the tunnel here without the control plane needing to know
	// whether anyone actually uses it. See internal/lanaccess's package doc
	// for why this is a second port rather than authenticating :8080 itself.
	lanMux := http.NewServeMux()
	server.RegisterRoutes(lanMux)

	lanAddr := lanListenAddr()

	go func() {
		log.Printf("LAN/tunnel listener on %s (access code: %s)", lanAddr, lanAccess.Password())

		if err := http.ListenAndServe(lanAddr, lanAccess.Middleware(httpapi.GzipMiddleware(lanMux))); err != nil {
			log.Printf("warning: LAN/tunnel listener failed: %v", err)
		}
	}()

	// Rotates the access code on its own every lanaccess.AutoRotateInterval,
	// the same drop-every-session rotation the Mobile menu's "New code"
	// button triggers by hand - see RunAutoRotate's doc for why it waits out
	// the first interval rather than rotating immediately.
	go lanAccess.RunAutoRotate(context.Background(), lanaccess.AutoRotateInterval)

	// IMPORTANT:
	// Ollama initialization must not block the HTTP server.
	go func() {
		if err := ollamaManager.EnsureReady(context.Background()); err != nil {
			log.Printf("warning: Ollama initialization failed: %v", err)
			return
		}

		log.Println("Ollama ready.")
	}()

	// On by default (see idleStopThreshold) - see internal/idlepause's
	// package doc for why this only ever does what a manual Stop already
	// does. Always started: Checker.Run itself is a no-op when Threshold is
	// non-positive, so FLOWERA_IDLE_STOP_MINUTES=0 opts out without any
	// different wiring here.
	idleThreshold := idleStopThreshold()
	idleAction := os.Getenv("FLOWERA_IDLE_ACTION")
	if idleAction == "" {
		idleAction = "pause"
	}

	if idleThreshold > 0 {
		log.Printf("Idle-pause: %sing desktops idle past %s. Set FLOWERA_IDLE_STOP_MINUTES=0 to disable.", idleAction, idleThreshold)
	} else {
		log.Println("Idle-pause: disabled (FLOWERA_IDLE_STOP_MINUTES=0).")
	}

	checker := &idlepause.Checker{Desktops: desktops, Tasks: tasksStore, Threshold: idleThreshold, Action: idleAction}
	server.IdleChecker = checker
	go checker.Run(context.Background())

	// Fires each recurring request set up on the requests page when its cron
	// expression next matches, through the same path a one-off Send takes
	// (Server.Submit). See internal/schedule's package doc.
	scheduler := &schedule.Runner{Store: schedules, Submitter: server}
	go scheduler.Run(context.Background())

	log.Printf("Desktop manager listening on %s", listenAddr)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           httpapi.GzipMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
