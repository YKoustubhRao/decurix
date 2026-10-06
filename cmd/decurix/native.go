package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// nativeMode reports whether this control plane's workers are native: private
// desktops of this computer rather than desktop containers. That is what a
// Windows build does - it needs no Docker and no Linux - unless
// DECURIX_NATIVE=0 asks for the container workers of a Docker Desktop that is
// running anyway.
func nativeMode() bool {
	return runtime.GOOS == "windows" && os.Getenv("DECURIX_NATIVE") != "0"
}

// nativeRuntime finds what a native worker runs: the Python that runs the
// agent, the agent itself, and TheBot's engine. The Windows app ships all
// three in a "runtime" folder beside decurix.exe; each can be named instead
// (DECURIX_NATIVE_PYTHON, DECURIX_NATIVE_AGENT, DECURIX_NATIVE_SOUL), which is
// how a checkout runs its own agent/ folder.
func nativeRuntime() (desktop.NativeRuntime, error) {
	beside := "runtime"
	if exe, err := os.Executable(); err == nil {
		beside = filepath.Join(filepath.Dir(exe), "runtime")
	}

	find := func(variable, shipped, what string) (string, error) {
		p := os.Getenv(variable)
		if p == "" {
			p = filepath.Join(beside, filepath.FromSlash(shipped))
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%s was not found at %s (set %s to where it is)", what, p, variable)
		}
		return p, nil
	}

	python, err := find("DECURIX_NATIVE_PYTHON", "python/pythonw.exe", "the Python that runs a worker's agent")
	if err != nil {
		return desktop.NativeRuntime{}, err
	}
	agent, err := find("DECURIX_NATIVE_AGENT", "agent", "the worker agent")
	if err != nil {
		return desktop.NativeRuntime{}, err
	}
	soul, err := find("DECURIX_NATIVE_SOUL", "soul.exe", "TheBot's engine")
	if err != nil {
		return desktop.NativeRuntime{}, err
	}

	ui, err := find("DECURIX_NATIVE_UI", "decurix-ui.exe", "the worker's screen helper")
	if err != nil {
		return desktop.NativeRuntime{}, err
	}

	files := os.Getenv("DECURIX_NATIVE_FILES")
	if files == "" {
		candidate := filepath.Join(beside, "decurix-files.exe")
		if _, err := os.Stat(candidate); err == nil {
			files = candidate
		}
	} else if _, err := os.Stat(files); err != nil {
		return desktop.NativeRuntime{}, fmt.Errorf("the worker's Files app was not found at %s: %w", files, err)
	}

	return desktop.NativeRuntime{
		Root:            filepath.Join(nativeStoreDir(), "workers"),
		Python:          python,
		AgentDir:        agent,
		Soul:            soul,
		UIHelper:        ui,
		FilesHelper:     files,
		ControlPlaneURL: "http://127.0.0.1:" + primaryPort,
		Helper:          nativeHelper,
	}, nil
}

// nativeStoreDir is where the large things a native control plane keeps go:
// the workers' folders, and the local model runtime with its models. Not the
// data directory itself, which on Windows is in the roaming profile - the
// database and keys belong there, gigabytes of model do not - but the local
// one beside it. A data directory someone named (DECURIX_DATA_DIR) holds
// everything, so a test or a second installation stays in one place.
func nativeStoreDir() string {
	if os.Getenv("DECURIX_DATA_DIR") == "" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "Decurix")
		}
	}

	return sqlitedb.DataDir()
}

// nativeLANAddr is where the access-code-gated listener binds on a person's
// own Windows computer: loopback, on the port addr (":8090") names. Listening
// on every interface, as a server does, makes Windows Firewall ask the person
// whether to allow it the first time the app starts - and a phone reaches the
// app through its tunnel, which connects from this computer itself.
// DECURIX_LAN_BIND=0.0.0.0 opens it to the local network for whoever wants
// that and will answer the prompt.
func nativeLANAddr(addr string) string {
	host := os.Getenv("DECURIX_LAN_BIND")
	if host == "" {
		host = "127.0.0.1"
	}

	return host + addr
}

// nativeHelper runs winhost's helper script for one private desktop: Windows
// PowerShell (the one every Windows has) on the script, written to the temp
// folder, with no console window of its own.
func nativeHelper(ctx context.Context, desktopName string) (*exec.Cmd, error) {
	ps := desktop.PowershellExe()
	if ps == "" {
		return nil, errors.New("Windows PowerShell was not found on this computer")
	}

	dir := filepath.Join(os.TempDir(), "decurix-winhost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare the desktop helper: %w", err)
	}

	script := filepath.Join(dir, "bridge.ps1")
	if old, err := os.ReadFile(script); err != nil || !bytes.Equal(old, winhost.Script) {
		if err := os.WriteFile(script, winhost.Script, 0o644); err != nil {
			return nil, fmt.Errorf("write the desktop helper: %w", err)
		}
	}

	// The helper as a program of its own when this computer has it built (it
	// is built the first time, in a few seconds): a third of the memory of a
	// PowerShell kept running for each worker.
	if program := winhost.ScreenProgram(ps, script, hideWindow); program != "" {
		cmd := exec.CommandContext(ctx, program, "-DesktopName", desktopName, "-Script", script)
		hideWindow(cmd)

		return cmd, nil
	}

	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-DesktopName", desktopName)
	hideWindow(cmd)

	return cmd, nil
}

// localModel prepares the model a native worker on the Ollama brain reasons
// with: an Ollama on this machine (the control plane's own when the person
// has none), with the chat model chosen for this machine and the embedding
// model pulled. It is done once, the first time anything needs it - not at
// every start, and never for a control plane whose workers all use a hosted
// model.
type localModel struct {
	mu    sync.Mutex
	ready bool

	// waiting are the callers being told how far the preparation is: the
	// one doing it and everyone waiting behind it - a worker created while
	// the model downloads shows the download, not a spinner.
	waitingMu sync.Mutex
	waiting   map[int]func(string)
	nextID    int
}

func (l *localModel) tell(message string) {
	log.Print("Local model: " + message)

	l.waitingMu.Lock()
	defer l.waitingMu.Unlock()

	for _, say := range l.waiting {
		say(message)
	}
}

// ensure blocks until the local model can be used, saying what it is waiting
// for. A failure is returned and tried again by the next caller.
func (l *localModel) ensure(ctx context.Context, say func(string)) error {
	if say != nil {
		l.waitingMu.Lock()
		if l.waiting == nil {
			l.waiting = map[int]func(string){}
		}
		l.nextID++
		id := l.nextID
		l.waiting[id] = say
		l.waitingMu.Unlock()

		defer func() {
			l.waitingMu.Lock()
			delete(l.waiting, id)
			l.waitingMu.Unlock()
		}()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.ready {
		return nil
	}

	told := l.tell

	url, err := ollama.EnsureNative(ctx, filepath.Join(nativeStoreDir(), "ollama"), told)
	if err != nil {
		return err
	}

	// What every worker started from here on is given, and what model
	// selection measures. The control plane's own Ollama is its to pull into.
	os.Setenv("OLLAMA_URL", url)
	if url == ollama.NativeURL {
		os.Setenv("DECURIX_MODEL_AUTOPULL", "1")
	}

	ollama.SelectModel(ctx)

	if err := ollama.EnsureModels(ctx, url, told, ollama.DesktopModel(), ollama.EmbedModel); err != nil {
		return err
	}

	l.ready = true
	told("ready (" + ollama.DesktopModel() + " at " + url + ")")

	return nil
}
