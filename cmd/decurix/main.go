package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/joho/godotenv"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/cftunnel"
	"github.com/YKoustubhRao/decurix/internal/claudeauth"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/devices"
	"github.com/YKoustubhRao/decurix/internal/grounder"
	"github.com/YKoustubhRao/decurix/internal/handoff"
	"github.com/YKoustubhRao/decurix/internal/httpapi"
	"github.com/YKoustubhRao/decurix/internal/idlepause"
	"github.com/YKoustubhRao/decurix/internal/keepawake"
	"github.com/YKoustubhRao/decurix/internal/knowledge"
	"github.com/YKoustubhRao/decurix/internal/lanaccess"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/outbox"
	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/resourcemon"
	"github.com/YKoustubhRao/decurix/internal/schedule"
	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/skillhub"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/stepimages"
	"github.com/YKoustubhRao/decurix/internal/suggest"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/tunnel"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/workqueue"
)

const (
	// primaryPort is the primary listener's port: unauthenticated, for this
	// machine and its own desktops only. Which addresses it binds is
	// primaryListenAddrs' decision.
	primaryPort = "8080"

	// defaultLANAddr is the second listener, gated by internal/lanaccess -
	// the one a Cloudflare tunnel or a direct LAN address should point at.
	// Overridable with DECURIX_LAN_PORT.
	defaultLANAddr = ":8090"

	// defaultIdleStopMinutes is internal/idlepause's threshold when
	// DECURIX_IDLE_STOP_MINUTES is unset - idle-pause is on by default.
	// DECURIX_IDLE_STOP_MINUTES=0 opts out.
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
		return "decurix"
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

// stepImagesMaxBytes reads DECURIX_STEP_IMAGES_MB, the disk budget for action
// screenshots; unset or invalid means stepimages.DefaultMaxBytes.
func stepImagesMaxBytes() int {
	megabytes, err := strconv.Atoi(os.Getenv("DECURIX_STEP_IMAGES_MB"))
	if err != nil || megabytes <= 0 {
		return stepimages.DefaultMaxBytes
	}

	return megabytes << 20
}

// inContainer reports whether this process runs inside a Docker container.
func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// primaryListenAddrs is where the unauthenticated primary listener binds.
//
//   - DECURIX_ADMIN_ADDR (comma-separated host:port list) overrides everything.
//   - In a container, every interface: the container is only as reachable as
//     what docker-compose.yaml publishes (loopback), and desktops reach it
//     over decurix-network.
//   - On the host, loopback plus the Docker bridge gateways, so desktops can
//     still call back through host.docker.internal but the LAN cannot reach
//     it. (On Docker Desktop host.docker.internal already lands on loopback;
//     a gateway address that cannot be bound there is skipped.)
//
// The first address is required; the rest are best effort.
func primaryListenAddrs(gateways func() []string) []string {
	if v := strings.TrimSpace(os.Getenv("DECURIX_ADMIN_ADDR")); v != "" {
		var addrs []string
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addrs = append(addrs, a)
			}
		}
		if len(addrs) > 0 {
			return addrs
		}
	}
	if inContainer() {
		return []string{":" + primaryPort}
	}
	addrs := []string{"127.0.0.1:" + primaryPort}
	for _, ip := range gateways() {
		addrs = append(addrs, net.JoinHostPort(ip, primaryPort))
	}
	return addrs
}

// lanListenAddr reads DECURIX_LAN_PORT, the port internal/lanaccess's gated
// listener binds; unset or invalid means defaultLANAddr.
func lanListenAddr() string {
	port := os.Getenv("DECURIX_LAN_PORT")
	if port == "" {
		return defaultLANAddr
	}

	if _, err := strconv.Atoi(port); err != nil {
		log.Printf("warning: DECURIX_LAN_PORT=%q is not a number; using %s", port, defaultLANAddr)
		return defaultLANAddr
	}

	return ":" + port
}

// lanPort returns the port of a listen address such as ":8090" - what the
// named tunnel routes to.
func lanPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 8090
	}

	n, err := strconv.Atoi(port)
	if err != nil {
		return 8090
	}

	return n
}

// idleStopThreshold reads DECURIX_IDLE_STOP_MINUTES, in minutes.
// internal/idlepause is on by default: unset or invalid means
// defaultIdleStopMinutes, not disabled. DECURIX_IDLE_STOP_MINUTES=0 (or any
// non-positive value) is the explicit opt-out - Checker.Run treats a
// non-positive Threshold as "do nothing" rather than this function needing
// its own enabled/disabled flag.
func idleStopThreshold() time.Duration {
	raw := os.Getenv("DECURIX_IDLE_STOP_MINUTES")
	if raw == "" {
		return defaultIdleStopMinutes * time.Minute
	}

	minutes, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("warning: DECURIX_IDLE_STOP_MINUTES=%q is not a number; using the default of %d minutes", raw, defaultIdleStopMinutes)
		return defaultIdleStopMinutes * time.Minute
	}

	if minutes <= 0 {
		return 0
	}

	return time.Duration(minutes) * time.Minute
}

// mailPollInterval is how often each connected mailbox is checked:
// DECURIX_MAIL_POLL_SECONDS, at least 15 (a provider rate-limits logins), else
// mailtrigger.DefaultPollInterval.
func mailPollInterval() time.Duration {
	raw := os.Getenv("DECURIX_MAIL_POLL_SECONDS")
	if raw == "" {
		return mailtrigger.DefaultPollInterval
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		log.Printf("warning: DECURIX_MAIL_POLL_SECONDS=%q is not a positive number; using %s", raw, mailtrigger.DefaultPollInterval)
		return mailtrigger.DefaultPollInterval
	}

	if seconds < 15 {
		seconds = 15
	}

	return time.Duration(seconds) * time.Second
}

// main loads configuration, sets up the Docker client, ensures required images/networks exist, registers HTTP routes, and starts the server.
func main() {
	showVersion := flag.Bool("version", false, "print the Decurix version and exit")
	buildImages := flag.Bool("build-images", false, "build the desktop images from the build context embedded in this binary, then exit")
	rebuild := flag.Bool("rebuild", false, "with -build-images, rebuild images that already exist instead of skipping them")
	imagesWhich := flag.String("images", "all", "with -build-images, which images: desktop, tools or all")
	cliMode := flag.Bool("cli", false, "run an interactive CLI against a running control plane instead of starting the server")
	cliURL := flag.String("url", "http://127.0.0.1:8080", "with -cli, the control plane's URL")
	phoneMode := flag.Bool("phone", false, `run as a phone link, in Termux on an Android phone: decurix --phone "<pairing link>" (or with no link, reconnect)`)
	phoneName := flag.String("phone-name", "", "with -phone, the name this phone is listed under (default: its model)")
	phoneSerial := flag.String("phone-serial", "", "with -phone, the adb device to use when there are several")
	restoreBackup := flag.String("restore-backup", "", "put a backup's database and key files back into the data dir (the control plane must be stopped), then exit")
	restoreForce := flag.Bool("force", false, "with -restore-backup, restore even though a control plane answers at -url")
	phoneSetup := flag.Bool("phone-setup", false, "set up this phone's link (what the phone link page's command runs): decurix --phone-setup BASE TOKEN")
	flag.Parse()

	if *showVersion {
		fmt.Printf("decurix %s\n", version)
		return
	}

	// -cli needs none of the setup below: no Docker client, no database -
	// it is a thin client for a control plane already running somewhere
	// else, reached over the same HTTP API the dashboard uses.
	if *cliMode {
		os.Exit(runCLI(*cliURL))
	}

	// -phone is the same kind of thin client, on a phone: no Docker, no
	// database, only an outbound link to a control plane (internal/phoneagent).
	if *phoneSetup {
		os.Exit(runPhoneSetup(flag.Arg(0), flag.Arg(1), *phoneSerial))
	}
	if *phoneMode {
		os.Exit(runPhone(flag.Arg(0), *phoneName, *phoneSerial))
	}

	// Restoring reads .env for DECURIX_DATA_DIR and nothing else.
	if *restoreBackup != "" {
		loadEnv()
		os.Exit(runRestore(*restoreBackup, sqlitedb.DataDir(), *cliURL, *restoreForce))
	}

	log.Printf("Decurix %s starting", version)

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
	// Host hops (privileged containers that run something on the host) left by an earlier run go first: their owner is
	// gone, and the Windows helper a hop runs keeps its app and its memory for as long as the hop lives.
	desktop.SweepHopsAtStart()
	// And the host-app helpers (with the apps they started) that ran through those hops: a Windows program started
	// through WSL outlives the hop that started it.
	httpapi.SweepHostHelpersAtStart()
	// The key that signs the launch tokens in a desktop's host-app launchers (internal/winhost). It lives in the data
	// directory so launchers already written keep working across restarts; a failure only disables those apps.
	hostGUISecret, err := winhost.LoadSecret(sqlitedb.DataDir())
	if err != nil {
		log.Printf("host apps: %v - Windows apps shown from the host are disabled", err)
	}
	desktops.HostGUISecret = hostGUISecret
	// Requested apps a desktop was created without (and the retry of ones the host could not be asked about yet) live
	// in memory; read them back from the desktops, which keep them for their agents. Its retries wait long enough for
	// the host-app checker that the HTTP server installs below.
	go desktops.RestoreMissingApps(context.Background())
	desktop.OptimizeHostKSM()
	ollamaManager := ollama.NewManager(cli, desktop.NetworkName)
	ollama.EnableWarm()
	// Chooses the chat model for this machine unless OLLAMA_MODEL names one.
	ollama.EnableAutoSelect()

	// Building all three images takes a long time and a lot of disk, so it is
	// an explicit request rather than something startup does on its own.
	if *buildImages {
		if err := desktops.BuildImages(ctx, os.Stdout, !*rebuild, *imagesWhich); err != nil {
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

	// Desktops created before the control plane moved off the LAN-facing
	// address keep calling it (see Manager.StaleControlPlane): say so once,
	// with what to do, rather than leaving them to fail silently.
	go func() {
		if stale := desktops.StaleControlPlane(context.Background()); len(stale) > 0 {
			log.Printf("warning: desktops %s call the control plane at an address it no longer serves them on; "+
				"recreate them (their environment is fixed when they are created)", strings.Join(stale, ", "))
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
	pairedDevices := must(devices.NewStore(db))
	lanAccess.SetDevices(pairedDevices)

	// The fixed link on the person's own domain, when they set one up
	// (internal/cftunnel). A key that cannot be written leaves the tokens
	// unsealed rather than the feature off.
	cfKey, err := sealed.LoadKey(sqlitedb.DataDir(), cftunnel.KeyFile)
	if err != nil {
		log.Printf("warning: %s: %v; the Cloudflare tokens will be stored unencrypted", cftunnel.KeyFile, err)
	}
	namedTunnel := must(cftunnel.NewManager(db, cfKey, lanPort(lanListenAddr())))
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

	server.Devices = pairedDevices
	server.DB = db
	server.SkillHub = must(skillhub.NewStore(db))
	server.Knowledge = must(knowledge.NewStore(db))
	server.Handoffs = server.NewHandoffService(must(handoff.NewStore(db)))
	go server.Handoffs.Run(context.Background())
	server.Queue = must(workqueue.NewStore(db))
	server.QueueDispatcher = server.NewQueueDispatcher(server.Queue, db)
	go server.QueueDispatcher.Run(context.Background(), 10*time.Second)
	server.Qualification = must(qualification.NewStore(db))
	server.SetAccess(must(access.NewStore(db)))
	for _, run := range must(server.Qualification.Interrupted()) {
		if err := tasksStore.CloseOpen("readiness-"+run.ID, "Readiness run interrupted by a control-plane restart; never automatically replayed."); err != nil {
			log.Fatalf("reconcile interrupted readiness task: %v", err)
		}
	}
	if err := server.SetQualificationOwner(sqlitedb.DataDir()); err != nil {
		log.Fatalf("create readiness owner key: %v", err)
	}
	// Qualification work is disposable. Never resume a process whose evidence
	// transaction was interrupted; startup revoked its grant in NewStore.
	qualificationCleanup, qualificationCleanupDone := context.WithTimeout(context.Background(), 30*time.Second)
	if err := desktops.CleanupQualification(qualificationCleanup); err != nil {
		log.Printf("warning: cleanup interrupted readiness containers: %v", err)
	}
	// Practice desktops are disposable the same way (docs/simulation.md).
	if err := desktops.CleanupPractice(qualificationCleanup); err != nil {
		log.Printf("warning: cleanup practice containers: %v", err)
	}
	qualificationCleanupDone()
	server.CFTunnel = namedTunnel

	// Notes of desktops deleted before deleting resolved them sat in "Needs
	// you" for good, with a Reply that had nowhere to go. Only when the list
	// is known: a failed list is not evidence that anything was deleted.
	go func() {
		all, err := desktops.List(context.Background())
		if err != nil {
			return
		}
		ids := make([]string, 0, len(all))
		for _, d := range all {
			ids = append(ids, d.ID)
		}
		if n := server.QueryNotes.ResolveOrphans(ids); n > 0 {
			log.Printf("Resolved %d open note(s) of desktops that no longer exist.", n)
		}
	}()
	// Linked Android phones (decurix --phone): their apps join the app list.
	server.SetPhones(phonehost.NewHub())
	// A phone that pairs is offered its phone link, set up from the phone
	// itself; Termux fetches the setup with a one-time token, not a cookie.
	lanAccess.SetAfterPair(server.PhoneLinkAfterPair)
	lanAccess.SetOpenPaths(httpapi.PhoneSetupOpenPaths...)
	server.HostGUISecret = hostGUISecret
	// The hosted models (Claude, Azure) a person set up on the dashboard,
	// keys sealed like the Cloudflare tokens. Never read from .env.
	llmKey, err := sealed.LoadKey(sqlitedb.DataDir(), llm.KeyFile)
	if err != nil {
		log.Printf("warning: %s: %v; the model API keys will be stored unencrypted", llm.KeyFile, err)
	}
	server.LLM = must(llm.NewHub(db, llmKey))
	// Simulate: practice runs of what a worker was taught, on a practice desktop that reaches only its broker
	// (docs/simulation.md). Its routes are the owner's, primary listener only.
	practiceSvc := &practice.Service{
		Store: server.Qualification, LLM: server.LLM, Offline: desktops, Workers: desktops, Sandbox: desktops, Tasks: tasksStore,
		Broker: &practice.Broker{LLM: server.LLM, Steps: func(w, t, text string) { _, _ = tasksStore.AddStep(w, t, tasks.Step{Text: text}) }},
		Rules: func(id string) string {
			var b strings.Builder
			for _, e := range server.Memory.List(id) {
				b.WriteString(e.Title + ": " + e.Summary + "\n")
			}
			return b.String()
		},
		Vision:     func(p string) bool { return server.LLM.Status()[p].Vision },
		FreeMemory: func() (uint64, error) { _, avail, err := ollama.AvailableMemory(); return avail, err },
		MaxTokens:  practice.MaxTokensFromEnv(),
	}
	brokerLn := &practice.BrokerListener{Network: desktops.PracticeBrokerEndpoint, Port: "8091", Handler: practiceSvc.Broker.Handler()}
	defer brokerLn.Close()
	practiceSvc.BrokerURL = brokerLn.URL
	practiceSubnets := &practice.SubnetCache{Lookup: desktops.PracticeSubnets}
	// A GUI-grounding model, when .env names one (DECURIX_GROUNDER_MODEL):
	// where on the screen a desktop's `gui click --see "..."` clicks.
	if g := grounder.FromEnv(ollama.DesktopURL()); g != nil {
		server.Grounder = g
		log.Printf("grounding model: %s (%s, %s)", g.Model, g.API, g.Coords)
	}
	// The Windows apps copied into VMs are kept running on the host, ready to show the moment one is opened.
	server.HostGUIDataDir = sqlitedb.DataDir()
	go server.RestoreHostApps(context.Background())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resMon := resourcemon.NewMonitor(desktops)
	server.ResourceMon = resMon
	go resMon.Run(ctx)

	// internal/dag's Runner needs Server itself as its Submitter
	// (SubmitTracked, which - unlike Submit - hands back the task ID a node's
	// ticket landed under), so it is built after Server and wired back onto
	// it rather than passed through NewServer.
	dagRunner := &dag.Runner{Store: dags, Submitter: server, Tasks: tasksStore}
	server.DagRunner = dagRunner
	go dagRunner.Run(ctx)

	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	server.RegisterLocalRoutes(mux)
	server.RegisterSimulationRoutes(mux, practiceSvc)
	server.RegisterAccessRoutes(mux)
	server.RegisterAccessLocalRoutes(mux)

	// The second listener is what a Cloudflare tunnel or a direct LAN
	// address should be pointed at: every route the primary listener serves,
	// gated behind lanAccess.Middleware. It always starts - the access code
	// gating it is what makes that safe - so build.sh/rebuild.sh can always
	// point the tunnel here without the control plane needing to know
	// whether anyone actually uses it. See internal/lanaccess's package doc
	// for why this is a second port rather than authenticating :8080 itself.
	lanMux := http.NewServeMux()
	server.RegisterRoutes(lanMux)
	server.RegisterAccessRoutes(lanMux)

	lanAddr := lanListenAddr()

	newServer := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}

	// A practice desktop reaches only its broker: both listeners refuse its subnet (practice.RefuseSubnets).
	lanServer := newServer(lanAddr, practice.RefuseSubnets(practiceSubnets.Subnets, lanAccess.Middleware(httpapi.GzipMiddleware(httpapi.MarkGated(lanMux)))))
	// Every listener, for the shutdown below; appended to only before it starts.
	activeServers := []*http.Server{lanServer}

	go func() {
		log.Printf("LAN/tunnel listener on %s (access code: %s)", lanAddr, lanAccess.Password())

		if err := lanServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("warning: LAN/tunnel listener failed: %v", err)
		}
	}()

	// Rotates the access code on its own every lanaccess.AutoRotateInterval,
	// the same drop-every-session rotation the Mobile menu's "New code"
	// button triggers by hand - see RunAutoRotate's doc for why it waits out
	// the first interval rather than rotating immediately.
	go lanAccess.RunAutoRotate(ctx, lanaccess.AutoRotateInterval)
	go namedTunnel.Run(ctx)

	// IMPORTANT:
	// Ollama initialization must not block the HTTP server.
	// With a hosted model set up, the local one is loaded only once some
	// desktop reasons with it (the Ollama brain, or an old ollama-harness
	// desktop) - a new desktop may well pick the hosted one. Measured on an
	// 8 GB host: the preload alone held 1.8 GB and left the machine swapping
	// for minutes with only hosted-model desktops running. Checked each time,
	// so a key typed in after startup counts.
	ollama.SetWarmNeeded(func() bool {
		return !server.LLM.AnyConfigured() || desktops.UsesLocalModel(context.Background())
	})

	go func() {
		if !ollama.Needed() {
			log.Println("Ollama: not preloading - a hosted model is configured and no desktop uses a local-model engine yet.")
			// Still choose the model (it reads /api/tags and /api/show, and
			// loads nothing): skipping this left ollama.Model's fallback as
			// the model a later Ollama-brain desktop was given, and the one
			// the top bar showed - a 24B model on an 8 GB host.
			ollama.SelectModel(ctx)
			return
		}

		if err := ollamaManager.EnsureReady(ctx); err != nil {
			log.Printf("warning: Ollama initialization failed: %v", err)
			return
		}

		log.Println("Ollama ready.")
	}()

	// On by default (see idleStopThreshold) - see internal/idlepause's
	// package doc for why this only ever does what a manual Stop already
	// does. Always started: Checker.Run itself is a no-op when Threshold is
	// non-positive, so DECURIX_IDLE_STOP_MINUTES=0 opts out without any
	// different wiring here.
	idleThreshold := idleStopThreshold()
	idleAction := os.Getenv("DECURIX_IDLE_ACTION")
	if idleAction == "" {
		idleAction = "pause"
	}

	if idleThreshold > 0 {
		log.Printf("Idle-pause: will %s desktops idle past %s. Set DECURIX_IDLE_STOP_MINUTES=0 to disable.", idleAction, idleThreshold)
	} else {
		log.Println("Idle-pause: disabled (DECURIX_IDLE_STOP_MINUTES=0).")
	}

	checker := &idlepause.Checker{Desktops: desktops, Tasks: tasksStore, Threshold: idleThreshold, Action: idleAction}
	server.IdleChecker = checker
	go checker.Run(ctx)

	// Fires each recurring request set up on the requests page when its cron
	// expression next matches, through the same path a one-off Send takes
	// (Server.Submit). See internal/schedule's package doc.
	scheduler := &schedule.Runner{Store: schedules, Submitter: server}
	go scheduler.Run(ctx)

	// Starts a desktop's task when a matching email arrives (the Email page):
	// polls each connected mailbox, and sends a job only to a desktop that is
	// free. See internal/mailtrigger's package doc.
	// Mailbox passwords are encrypted in the database with a key kept beside
	// it (mailtrigger.KeyFile). Without the key they cannot be stored at all
	// rather than stored readable.
	mailKey, err := mailtrigger.LoadKey(sqlitedb.DataDir())
	if err != nil {
		log.Fatalf("email triggers: loading the password key: %v", err)
	}

	mail := must(mailtrigger.NewStore(db, mailKey))
	mailRunner := &mailtrigger.Runner{Store: mail, Submitter: server, PollInterval: mailPollInterval()}
	server.Mail = mail
	server.MailRunner = mailRunner

	// Emails agents drafted, sent only when a person presses Send.
	drafts := must(outbox.New(db))
	if err := drafts.RecoverInterrupted(); err != nil {
		log.Printf("outbox: %v", err)
	}
	server.Outbox = drafts
	go mailRunner.Run(ctx)

	// Keeps this machine from idle-sleeping while a mailbox is watched or a
	// schedule is waiting - nothing runs on a sleeping machine. Released the
	// moment there is nothing, and on the way out. In a container it cannot
	// reach the host, and the script that started Decurix holds it instead
	// (GET /api/keep-awake). See internal/keepawake.
	keeper := &keepawake.Keeper{Wanted: func() []string {
		var reasons []string
		if mail.Active() {
			reasons = append(reasons, "mailboxes are watched")
		}
		if mail.Waiting() {
			reasons = append(reasons, "email-triggered tasks are waiting to be sent")
		}
		if schedules.Active() || dags.Active() {
			reasons = append(reasons, "schedules are waiting to run")
		}
		if tasksStore.Working() {
			reasons = append(reasons, "a desktop is working on a task")
		}
		return reasons
	}}
	server.KeepAwake = keeper
	go keeper.Run(ctx)

	addrs := primaryListenAddrs(func() []string {
		// Only Linux has Docker's bridge gateways as addresses of its own to bind. On macOS and Windows Docker runs in
		// a VM: those addresses exist only inside it, binding them fails, and host.docker.internal already lands on
		// loopback there.
		if runtime.GOOS != "linux" {
			return nil
		}
		return desktops.GatewayIPs(ctx)
	})
	log.Printf("Desktop manager listening on %s", strings.Join(addrs, ", "))

	handler := practice.RefuseSubnets(practiceSubnets.Subnets, httpapi.RefuseForwarded(httpapi.GzipMiddleware(mux)))

	for _, addr := range addrs[1:] {
		s := newServer(addr, handler)
		activeServers = append(activeServers, s)
		go func(srv *http.Server, a string) {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("warning: primary listener on %s failed: %v", a, err)
			}
		}(s, addr)
	}

	primarySrv := newServer(addrs[0], handler)
	activeServers = append(activeServers, primarySrv)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for _, s := range activeServers {
			wg.Add(1)
			go func(srv *http.Server) {
				defer wg.Done()
				_ = srv.Shutdown(shutdownCtx)
			}(s)
		}
		wg.Wait()
	}()

	if err := primarySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
