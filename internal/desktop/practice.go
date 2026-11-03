package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Practice desktops (docs/simulation.md) run a worker's own image, entrypoint and
// agent against stand-ins on an internal Docker network. Nothing of the person's
// machine is mounted, the network has no route out, and every container carries
// PracticeLabel so CleanupPractice can sweep it after a restart.
const (
	// PracticeNetworkName is the internal network a practice desktop, its mock and the broker share.
	PracticeNetworkName = "decurix-practice"
	// PracticeLabel marks every container and network a practice run made.
	PracticeLabel = "decurix-practice"
	// PracticeMockAlias is the mock app's name on the practice network; the browser maps recorded hosts to it.
	PracticeMockAlias = "practice-mock"
	// PracticeMockPort is where the mock app listens inside its container.
	PracticeMockPort = "8000"
	// PracticeWorkDir is the scratch folder a case's inputs are copied into, one sub-folder per case.
	PracticeWorkDir = "/home/headless/work/practice"
	// practiceDecurixDir is the agent's state folder the pinned snapshot is restored into.
	practiceDecurixDir = "/home/headless/.decurix"
	// practiceMaxMemoryMB caps a practice desktop's memory whatever the worker's own setting.
	practiceMaxMemoryMB = 2048
	// practiceMaxTar bounds a copied agent tree or snapshot.
	practiceMaxTar = 64 << 20
)

// ErrNoSecret is why a desktop is not made when the control plane could not load its secret: the desktop would get
// no agent token, and its agent would refuse (or, built before tokens, accept) every request.
var ErrNoSecret = errors.New("the control plane could not load its secret (hostgui.key in the data folder), so a desktop cannot be made safely; check the data folder and restart Decurix")

// PracticeSnapshotParts are the agent state folders a case starts from, restored before every case.
var PracticeSnapshotParts = []string{"skills", "experience", "playbooks", "memory"}

// practiceResetPaths are cleared before every case. All of ~/.decurix goes, not a list of the folders known today:
// whatever the agent or soul learned or kept in a case (methods, app guides, procedures, recordings, workflows, soul's
// workspace ~/.decurix/ai with its memory) must not reach the next case, or a suite's repeats are one learning curve
// rather than independent samples (review EVAL-1, B1). Nothing in it is needed to run: the desktop's identity and
// agent token are its environment, the gate's key lives in /var/lib/decurix-gate, the start script makes soul's
// workspace again, and the run's snapshot (PracticeSnapshotParts; an eval's is empty) is restored after the clear.
// The browser profiles and the whole work folder go too: a case may have saved outside its own folder.
var practiceResetPaths = []string{
	"/home/headless/.config/chromium", "/home/headless/.config/google-chrome", "/home/headless/work", practiceDecurixDir,
}

// PracticeResetPaths lists what ResetPractice clears before every case, for runners and their tests.
func PracticeResetPaths() []string { return append([]string{}, practiceResetPaths...) }

// practiceResetScript ends every process of the worker first (the container restarts next, so nothing it runs may
// write after the clear), then clears the paths it is given and the agent's task files in /tmp.
const practiceResetScript = `kill -KILL -1 2>/dev/null; rm -rf -- "$@" /tmp/decurix-* /tmp/decurix.*`

var (
	// practiceHostPattern is a host name the browser may map to the mock: no quotes, spaces or ports.
	practiceHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)
	// practiceSecretKey names inherited image ENV that is emptied in a practice desktop.
	practiceSecretKey = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASS|CREDENTIAL|OLLAMA)`)
	practiceRunID     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// PracticeSpec is everything a practice desktop is made from. The worker's own
// desktop is never touched: its agent tree and state are copied in as tars.
type PracticeSpec struct {
	RunID     string
	Image     string
	Harness   string
	Brain     string
	Model     string
	Vision    bool
	MemoryMB  int64
	BrokerURL string
	// Hosts are the recorded web hosts the browser resolves to the mock app.
	Hosts []string
	// AgentTree is a tar of /opt/decurix_agent as PracticeAgentTree read it from the worker.
	AgentTree []byte
	// Snapshot is a tar of the agent's state folders (PracticeSnapshot), restored before each case.
	Snapshot []byte
}

// PracticeDesktop is a created practice desktop.
type PracticeDesktop struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	IP          string `json:"ip"`
}

// AgentURL is where the control plane reaches this practice desktop's agent.
func (d PracticeDesktop) AgentURL() string {
	return "http://" + d.IP + ":" + vmAgentPort
}

// PracticeDesktopID is the identity a practice desktop's agent reports itself as.
func PracticeDesktopID(runID string) string {
	return "practice-" + runID
}

// EnsurePracticeNetwork creates the internal practice network when it is missing
// and returns its subnet and gateway. An internal network has no route out.
func (m *Manager) EnsurePracticeNetwork(ctx context.Context) (netip.Prefix, netip.Addr, error) {
	inspect, err := m.client.NetworkInspect(ctx, PracticeNetworkName, network.InspectOptions{})
	if err != nil {
		if !client.IsErrNotFound(err) {
			return netip.Prefix{}, netip.Addr{}, err
		}
		if _, err = m.client.NetworkCreate(ctx, PracticeNetworkName, network.CreateOptions{
			Driver: "bridge", Internal: true, Labels: map[string]string{PracticeLabel: "true"},
		}); err != nil {
			return netip.Prefix{}, netip.Addr{}, fmt.Errorf("create practice network: %w", err)
		}
		if inspect, err = m.client.NetworkInspect(ctx, PracticeNetworkName, network.InspectOptions{}); err != nil {
			return netip.Prefix{}, netip.Addr{}, err
		}
	}
	if !inspect.Internal {
		return netip.Prefix{}, netip.Addr{}, errors.New("the practice network exists but is not internal; remove it")
	}
	for _, cfg := range inspect.IPAM.Config {
		subnet, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil || !subnet.Addr().Is4() {
			continue
		}
		gateway, _ := netip.ParseAddr(cfg.Gateway)
		return subnet, gateway, nil
	}
	return netip.Prefix{}, netip.Addr{}, errors.New("the practice network has no IPv4 subnet")
}

// PracticeBrokerEndpoint ensures the practice network and returns its subnet and
// the address the practice broker binds. On a host install that is the
// network's gateway. When the control plane itself runs in a container (the
// app's compose stack) the gateway is not its own address, so its container
// joins the practice network and binds its address there; both listeners refuse
// the practice subnet (practice.RefuseSubnets), so a practice desktop reaches
// only the broker.
func (m *Manager) PracticeBrokerEndpoint(ctx context.Context) (netip.Prefix, netip.Addr, error) {
	subnet, gateway, err := m.EnsurePracticeNetwork(ctx)
	if err != nil || !controlPlaneInContainer() {
		return subnet, gateway, err
	}
	self, err := os.Hostname()
	if err != nil {
		return subnet, netip.Addr{}, err
	}
	inspect, err := m.client.ContainerInspect(ctx, self)
	if err != nil {
		return subnet, netip.Addr{}, fmt.Errorf("find the control plane's own container: %w", err)
	}
	if ep := practiceEndpoint(inspect); ep == nil || ep.IPAddress == "" {
		if err := m.client.NetworkConnect(ctx, PracticeNetworkName, inspect.ID, nil); err != nil {
			return subnet, netip.Addr{}, fmt.Errorf("join the practice network: %w", err)
		}
		if inspect, err = m.client.ContainerInspect(ctx, inspect.ID); err != nil {
			return subnet, netip.Addr{}, err
		}
	}
	ep := practiceEndpoint(inspect)
	if ep == nil {
		return subnet, netip.Addr{}, errors.New("the control plane did not join the practice network")
	}
	addr, err := netip.ParseAddr(ep.IPAddress)
	if err != nil || !subnet.Contains(addr) {
		return subnet, netip.Addr{}, errors.New("the control plane has no address on the practice network")
	}
	return subnet, addr, nil
}

// leavePracticeNetwork disconnects the control plane's own container from the
// practice network, so the network can be removed; nothing on a host install.
func (m *Manager) leavePracticeNetwork(ctx context.Context) {
	if !controlPlaneInContainer() {
		return
	}
	if self, err := os.Hostname(); err == nil {
		_ = m.client.NetworkDisconnect(ctx, PracticeNetworkName, self, true)
	}
}

// practiceEndpoint is a container's endpoint on the practice network, or nil.
func practiceEndpoint(inspect container.InspectResponse) *network.EndpointSettings {
	if inspect.NetworkSettings == nil {
		return nil
	}
	return inspect.NetworkSettings.Networks[PracticeNetworkName]
}

// controlPlaneInContainer reports whether this process runs in a container.
func controlPlaneInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// PracticeSubnets returns the practice network's address ranges, for refusing
// them on the primary and gated listeners; nil when the network does not exist.
func (m *Manager) PracticeSubnets(ctx context.Context) []netip.Prefix {
	inspect, err := m.client.NetworkInspect(ctx, PracticeNetworkName, network.InspectOptions{})
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, cfg := range inspect.IPAM.Config {
		if p, err := netip.ParsePrefix(cfg.Subnet); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// copyOut reads one path of a container as a tar, bounded, by Docker's root-side copy. Only for a fixed root-owned
// path (/opt, /etc/passwd) or a container that has never run: never a path under a worker's home (homefs.go).
func (m *Manager) copyOut(ctx context.Context, containerID, from string, limit int64) ([]byte, error) {
	rc, _, err := m.client.CopyFromContainer(ctx, containerID, from)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", from, limit)
	}
	return b, nil
}

// TarFingerprint hashes a tar's regular files by name and content, in name
// order, so the same tree gives the same hash however Docker ordered it.
func TarFingerprint(b []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(b))
	type entry struct{ name, sum string }
	var entries []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg || strings.Contains(h.Name, "__pycache__") {
			continue
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, tr); err != nil {
			return "", err
		}
		entries = append(entries, entry{h.Name, hex.EncodeToString(sum.Sum(nil))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	all := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(all, "%s\x00%s\n", e.name, e.sum)
	}
	return hex.EncodeToString(all.Sum(nil)), nil
}

// PracticeAgentTree copies the worker's /opt/decurix_agent as a tar and its
// fingerprint, so practice tests the code production runs (the dev loop copies
// agent code over the image's).
func (m *Manager) PracticeAgentTree(ctx context.Context, workerID string) ([]byte, string, error) {
	found, err := m.Get(ctx, workerID)
	if err != nil {
		return nil, "", err
	}
	b, err := m.copyOut(ctx, found.ContainerID, "/opt/decurix_agent", practiceMaxTar)
	if err != nil {
		return nil, "", err
	}
	sum, err := TarFingerprint(b)
	return b, sum, err
}

// PracticeSnapshot copies the worker's agent state folders (PracticeSnapshotParts)
// into one tar rooted at ~/.decurix, and its fingerprint. A missing folder is skipped.
func (m *Manager) PracticeSnapshot(ctx context.Context, workerID string) ([]byte, string, error) {
	found, err := m.Get(ctx, workerID)
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	total := int64(0)
	for _, part := range PracticeSnapshotParts {
		// The real worker's home, so read as the worker (homefs.go), never by Docker's root-side copy (N-B1).
		b, err := m.readHomeBytes(ctx, found.ID, practiceDecurixDir+"/"+part, practiceMaxTar)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, "", err
		}
		tr := tar.NewReader(bytes.NewReader(b))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, "", err
			}
			// Only folders and regular files: a link could point out of the folder.
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
				continue
			}
			total += h.Size
			if total > practiceMaxTar {
				return nil, "", errors.New("the worker's state is too large to practise from")
			}
			body, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return nil, "", err
			}
			if err := tw.WriteHeader(&tar.Header{Name: h.Name, Typeflag: h.Typeflag, Mode: h.Mode & 0o755, Size: int64(len(body)), ModTime: h.ModTime}); err != nil {
				return nil, "", err
			}
			if _, err := tw.Write(body); err != nil {
				return nil, "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum, err := TarFingerprint(out.Bytes())
	return out.Bytes(), sum, err
}

// practiceBrowserFlags is the shell line appended to the image's browser launch
// flags. The launcher sources the file with its own arguments, so `set --` adds
// one argument with spaces intact (CHROMIUM_FLAGS is word-split).
func practiceBrowserFlags(hosts []string) (string, error) {
	if len(hosts) == 0 {
		return "", nil
	}
	rules := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !practiceHostPattern.MatchString(h) {
			return "", fmt.Errorf("not a host name: %q", h)
		}
		rules = append(rules, "MAP "+h+" "+PracticeMockAlias+":"+PracticeMockPort)
	}
	return "\n# Practice desktop: recorded hosts reach the mock app.\nset -- '--host-resolver-rules=" + strings.Join(rules, ",") + "' \"$@\"\n", nil
}

// practiceEnvironment keeps the image's own ENV (its startup needs it) but
// empties anything secret-shaped or pointing at the shared model, then adds
// only what a practice desktop needs. No key or Claude login enters it.
func practiceEnvironment(inherited []string, spec PracticeSpec, vncPassword string) []string {
	out := []string{}
	for _, item := range inherited {
		key, _, ok := strings.Cut(item, "=")
		if ok && practiceSecretKey.MatchString(key) {
			out = append(out, key+"=")
		}
	}
	broker := strings.TrimRight(spec.BrokerURL, "/")
	name := PracticeDesktopID(spec.RunID)
	out = append(out,
		"VNC_PW="+vncPassword, "VNC_RESOLUTION=1280x720",
		"DESKTOP_ID="+name, "DESKTOP_NAME="+name,
		"CONTROL_PLANE_URL="+broker,
		"AGENT_HARNESS="+spec.Harness,
		"DECURIX_BRAIN="+spec.Brain,
		"DECURIX_PRACTICE=1",
		GateRequiredEnv+"=1",
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1",
	)
	if (Brain{Kind: spec.Brain}).Hosted() {
		vision := "0"
		if spec.Vision {
			vision = "1"
		}
		out = append(out,
			"LLM_PROVIDER=openai",
			"OPENAI_BASE_URL="+broker+"/api/llm/"+spec.Brain,
			"OPENAI_API_KEY=held-by-the-control-plane",
			"OPENAI_MODEL=default",
			"DECURIX_LLM="+spec.Brain,
			"DECURIX_BRAIN_MODEL="+spec.Model,
			"DECURIX_BRAIN_VISION="+vision,
		)
	}
	return out
}

// practiceConfigs is a practice desktop's boundary: the image's entrypoint,
// the internal network only, no binds, no host gateway, the normal desktop
// seccomp profile, and capped resources.
func practiceConfigs(spec PracticeSpec, env, securityOpt []string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	memoryMB := spec.MemoryMB
	if memoryMB <= 0 || memoryMB > practiceMaxMemoryMB {
		memoryMB = practiceMaxMemoryMB
	}
	resources := desktopResources(2, memoryMB)
	resources.MemorySwap = resources.Memory
	resources.Ulimits = desktopUlimits()
	pids := int64(512)
	resources.PidsLimit = &pids
	name := PracticeDesktopID(spec.RunID)
	return &container.Config{
			Image: spec.Image, Env: env, Hostname: name,
			Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": spec.RunID},
		}, &container.HostConfig{
			NetworkMode: container.NetworkMode(PracticeNetworkName),
			// The worker's own desktop's options, no-new-privileges included as on every desktop (B-21): the
			// image's startup no longer uses sudo (the entrypoint drops privileges with setpriv; B-35 corrected the
			// old comment). The boundary is also the internal network, no binds and no host gateway.
			SecurityOpt: append(append([]string{}, securityOpt...), "no-new-privileges:true"),
			ShmSize:     256 << 20,
			Resources:   resources,
			Tmpfs:       map[string]string{"/tmp": "rw,nosuid,size=256m,mode=1777", "/run": "rw,nosuid,size=64m,mode=755"},
			LogConfig:   container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
		}, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{PracticeNetworkName: {}}}
}

// practiceImage checks the pinned image as RunQualification does: the same ID,
// Linux, and no declared VOLUME (an anonymous volume would escape the inventory).
func (m *Manager) practiceImage(ctx context.Context, image string) ([]string, error) {
	inspect, _, err := m.client.ImageInspectWithRaw(ctx, image)
	if err != nil {
		return nil, err
	}
	if inspect.ID != image {
		return nil, errors.New("the worker's image changed since the suite was pinned")
	}
	if inspect.Config == nil || len(inspect.Config.Volumes) != 0 || inspect.Os != "linux" {
		return nil, errors.New("practice needs a Linux image that declares no volumes")
	}
	return inspect.Config.Env, nil
}

// CreatePractice makes and starts one practice desktop. It copies in the
// worker's agent tree and state snapshot and the browser's host mapping before
// the first start; it never migrates a home, copies launchers or host apps,
// seeds credentials or binds a shared folder.
func (m *Manager) CreatePractice(ctx context.Context, spec PracticeSpec) (d PracticeDesktop, err error) {
	if !practiceRunID.MatchString(spec.RunID) || !strings.HasPrefix(spec.Image, "sha256:") || spec.BrokerURL == "" {
		return d, qualification.ErrInvalid
	}
	// A practice desktop gets the same per-desktop token as any desktop, or is not made: without the secret its agent
	// would have no token, refuse every request the run sends it and the run would fail with no reason (review
	// 2026-11-02, second round).
	if len(m.HostGUISecret) == 0 {
		return d, ErrNoSecret
	}
	flags, err := practiceBrowserFlags(spec.Hosts)
	if err != nil {
		return d, err
	}
	if _, _, err = m.EnsurePracticeNetwork(ctx); err != nil {
		return d, err
	}
	inherited, err := m.practiceImage(ctx, spec.Image)
	if err != nil {
		return d, err
	}
	env := practiceEnvironment(inherited, spec, practicePassword())
	// A practice desktop's agent takes requests only with its token, like every desktop's (agenttoken.go).
	token := m.AgentToken(PracticeDesktopID(spec.RunID))
	if token == "" {
		return d, ErrNoSecret
	}
	env = append(env, AgentTokenEnv+"="+token)
	config, host, net := practiceConfigs(spec, env, m.desktopSecurityOpt(ctx))
	created, err := m.client.ContainerCreate(ctx, config, host, net, nil, PracticeDesktopID(spec.RunID))
	if err != nil {
		return d, err
	}
	d = PracticeDesktop{ContainerID: created.ID, Name: PracticeDesktopID(spec.RunID)}
	defer func() {
		if err != nil {
			_ = m.RemovePractice(context.WithoutCancel(ctx), created.ID)
		}
	}()
	// A practice desktop has the same approval gate as the worker's own: practice tests what will run, and without a
	// gate its agent would keep approvals in a store the worker can write. The practice broker never grants one.
	if err = m.prepareGate(ctx, created.ID, m.GateKey(PracticeDesktopID(spec.RunID))); err != nil {
		return d, fmt.Errorf("give the practice desktop its approval gate: %w", err)
	}
	if flags != "" {
		if err = m.appendBrowserFlags(ctx, created.ID, flags); err != nil {
			return d, err
		}
	}
	// Root-side copies into a practice desktop that has never started: nothing in it is the worker's yet.
	if len(spec.AgentTree) > 0 {
		if err = m.client.CopyToContainer(ctx, created.ID, "/opt", bytes.NewReader(spec.AgentTree), container.CopyToContainerOptions{}); err != nil {
			return d, fmt.Errorf("copy the agent tree: %w", err)
		}
	}
	if len(spec.Snapshot) > 0 {
		if err = m.restoreSnapshot(ctx, created.ID, spec.Snapshot); err != nil {
			return d, fmt.Errorf("restore the worker's state: %w", err)
		}
	}
	if err = m.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return d, err
	}
	d.IP, err = m.practiceIP(ctx, created.ID)
	return d, err
}

// appendBrowserFlags rewrites the image's browser launch flags, which the agent's
// start script installs into $HOME on every start, so a restart keeps the mapping.
func (m *Manager) appendBrowserFlags(ctx context.Context, id, flags string) error {
	const init = "/opt/decurix-chromium.init"
	b, err := m.copyOut(ctx, id, init, 1<<20)
	if err != nil {
		return fmt.Errorf("read the browser flags: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	h, err := tr.Next()
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		return err
	}
	body = append(body, []byte(flags)...)
	return m.client.CopyToContainer(ctx, id, "/opt", tarOf(map[string][]byte{path.Base(init): body}, h.Mode, 0), container.CopyToContainerOptions{})
}

// tarOf packs flat files into a tar with one mode and owner.
// restoreSnapshot copies the worker's state tar (paths relative to its .decurix
// folder) into a practice desktop. A desktop that never started has no .decurix
// folder and Docker will not copy into a missing one, so the tar is rewritten
// under .decurix/ with the folder's own entry, owned by the desktop user, and
// copied into the home folder. It is a root-side copy: on creation the desktop has never run; on a reset
// (ResetPractice) the practice worker has, and its ~/.decurix could be a link - but the practice home has no volume a
// helper could mount and an exec needs the desktop running, so it stays, and what it can write is confined to the
// snapshot's own parts' names under that folder. Moving the reset's restore before the stop, as the worker, is the
// follow-up (it needs a live practice run to check).
func (m *Manager) restoreSnapshot(ctx context.Context, id string, snapshot []byte) error {
	under, err := underFolder(snapshot, path.Base(practiceDecurixDir), practiceUID)
	if err != nil {
		return err
	}
	return m.client.CopyToContainer(ctx, id, path.Dir(practiceDecurixDir), bytes.NewReader(under), container.CopyToContainerOptions{CopyUIDGID: true})
}

// practiceUID is the desktop user's uid and gid in the desktop image.
const practiceUID = 1001

// underFolder rewrites a tar so every entry sits under folder/, led by the
// folder's own directory entry owned by uid.
func underFolder(archive []byte, folder string, uid int) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: folder + "/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: uid, ModTime: time.Unix(0, 0)}); err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if name == "" || name == "." {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			name += "/"
		}
		h.Name = folder + "/" + name
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func tarOf(files map[string][]byte, mode int64, uid int) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: mode, Uid: uid, Gid: uid, Size: int64(len(files[n])), ModTime: time.Unix(0, 0)})
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	return &buf
}

func (m *Manager) practiceIP(ctx context.Context, id string) (string, error) {
	inspect, err := m.client.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	if inspect.NetworkSettings != nil {
		if ep := inspect.NetworkSettings.Networks[PracticeNetworkName]; ep != nil && ep.IPAddress != "" {
			return ep.IPAddress, nil
		}
	}
	return "", errors.New("the practice desktop has no address on the practice network")
}

func practicePassword() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ResetPractice returns a practice desktop to the run's starting state before a
// case: it ends the worker's processes, clears all of its agent state, the browser
// profile and the work folder, restarts the container, and restores the pinned
// snapshot while it is stopped.
func (m *Manager) ResetPractice(ctx context.Context, id string, snapshot []byte) error {
	cmd := append([]string{"/bin/sh", "-c", practiceResetScript, "reset"}, practiceResetPaths...)
	if err := m.practiceExec(ctx, id, cmd); err != nil {
		return fmt.Errorf("clear the practice desktop: %w", err)
	}
	timeout := 10
	if err := m.client.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		return err
	}
	if len(snapshot) > 0 {
		if err := m.restoreSnapshot(ctx, id, snapshot); err != nil {
			return fmt.Errorf("restore the worker's state: %w", err)
		}
	}
	return m.client.ContainerStart(ctx, id, container.StartOptions{})
}

// practiceExec runs a command as the desktop's user and waits for it to end.
func (m *Manager) practiceExec(ctx context.Context, id string, cmd []string) error {
	created, err := m.client.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: workerCmd(cmd...), User: "headless"})
	if err != nil {
		return err
	}
	if err := m.client.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return err
	}
	for i := 0; i < 300; i++ {
		state, err := m.client.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return err
		}
		if !state.Running {
			if state.ExitCode != 0 {
				return fmt.Errorf("exit %d", state.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("the command did not finish")
}

// CopyPracticeInputs puts a case's input files into PracticeWorkDir/<case>/,
// owned by the desktop's user.
func (m *Manager) CopyPracticeInputs(ctx context.Context, id, caseID string, files map[string][]byte) error {
	if !practiceRunID.MatchString(caseID) {
		return qualification.ErrInvalid
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, dir := range []string{"work/", "work/practice/", "work/practice/" + caseID + "/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		if !qualification.FileName(n) {
			return qualification.ErrInvalid
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: "work/practice/" + caseID + "/" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n])), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// The practice worker has run in this home, so the inputs go in as that worker (homefs.go), never root-side.
	target, done, err := m.homeAccessOf(ctx, id, id)
	if err != nil {
		return err
	}
	defer done()
	return m.execTarIn(ctx, target, homeDir, &buf, "--no-overwrite-dir")
}

// CollectPractice reads a case's folder after the desktop is paused. It stays a root-side copy on purpose: the
// worker must stay frozen while its work is collected, and an exec needs it running. What a link there could reach
// is the practice container's own (a disposable copy, its own key), and only flat regular files count as evidence. Inputs that
// changed or went missing are listed, new files are outputs; anything that is not
// a flat regular file, or over the limits, is a problem rather than evidence.
func (m *Manager) CollectPractice(ctx context.Context, id, caseID string, inputs map[string][]byte) (outputs map[string][]byte, changed, problems []string, err error) {
	b, err := m.copyOut(ctx, id, PracticeWorkDir+"/"+caseID, qualification.MaxTotalBytes+1<<20)
	if err != nil {
		if client.IsErrNotFound(err) {
			for n := range inputs {
				changed = append(changed, n)
			}
			sort.Strings(changed)
			return map[string][]byte{}, changed, []string{"The case folder was removed."}, nil
		}
		return nil, nil, nil, err
	}
	outputs = map[string][]byte{}
	seen := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(b))
	total := int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, err
		}
		name := strings.TrimPrefix(strings.TrimPrefix(h.Name, caseID), "/")
		if name == "" || strings.TrimSuffix(name, "/") == "" {
			continue
		}
		if h.Typeflag != tar.TypeReg || strings.Contains(strings.TrimSuffix(name, "/"), "/") || !qualification.FileName(name) || h.Size > qualification.MaxFileBytes {
			problems = append(problems, fmt.Sprintf("Unexpected item in the case folder: %s.", strings.TrimSuffix(name, "/")))
			continue
		}
		total += h.Size
		if total > qualification.MaxTotalBytes {
			problems = append(problems, "The case folder exceeds the evidence limit.")
			break
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, nil, nil, err
		}
		seen[name] = true
		if in, ok := inputs[name]; ok {
			if !bytes.Equal(in, body) {
				changed = append(changed, name)
			}
			continue
		}
		outputs[name] = body
	}
	for n := range inputs {
		if !seen[n] {
			changed = append(changed, n)
		}
	}
	sort.Strings(changed)
	return outputs, changed, problems, nil
}

// practiceMockConfigs is the mock app's boundary: qualification's (read-only
// root, tmpfs /work, no capabilities, small limits) on the practice network, under
// PracticeMockAlias. It waits for its files, then serves the generated app.
func practiceMockConfigs(image, runID string, inherited []string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	config, host := qualificationConfigs(qualification.Definition{Image: image})
	config.Env = qualificationEnvironment(inherited)
	config.Entrypoint = []string{"/bin/sh", "-c", "while [ ! -f /work/ready ]; do sleep 0.2; done; exec /usr/bin/python3 /work/mock_app.py --port " + PracticeMockPort + " --fixture /work/fixture.json --state /work/state.json"}
	config.Cmd = nil
	config.Labels = map[string]string{PracticeLabel: "true", "decurix-practice-run": runID, "decurix-practice-mock": "true"}
	host.NetworkMode = container.NetworkMode(PracticeNetworkName)
	host.Resources.Memory, host.Resources.MemorySwap = 128<<20, 128<<20
	pids := int64(32)
	host.Resources.PidsLimit = &pids
	return config, host, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{PracticeNetworkName: {Aliases: []string{PracticeMockAlias}}}}
}

// StartPracticeMock starts a fresh mock app for one case and copies its source
// and fixture in. Its state.json is outside the practice desktop, where the
// worker cannot rewrite it.
func (m *Manager) StartPracticeMock(ctx context.Context, image, runID string, source, fixture []byte) (id string, err error) {
	if !practiceRunID.MatchString(runID) || len(source) == 0 {
		return "", qualification.ErrInvalid
	}
	inherited, err := m.practiceImage(ctx, image)
	if err != nil {
		return "", err
	}
	if len(fixture) == 0 {
		fixture = []byte("{}")
	}
	config, host, net := practiceMockConfigs(image, runID, inherited)
	created, err := m.client.ContainerCreate(ctx, config, host, net, nil, "")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = m.RemovePractice(context.WithoutCancel(ctx), created.ID)
		}
	}()
	if err = m.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	// ready goes last, so the entrypoint never starts on a half-copied folder.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		body []byte
	}{{"mock_app.py", source}, {"fixture.json", fixture}, {"ready", nil}} {
		if err = tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1001, Gid: 1001, Size: int64(len(f.body)), ModTime: time.Unix(0, 0)}); err != nil {
			return "", err
		}
		if _, err = tw.Write(f.body); err != nil {
			return "", err
		}
	}
	if err = tw.Close(); err != nil {
		return "", err
	}
	if err = m.execTarIn(ctx, created.ID, "/work", &buf); err != nil {
		return "", err
	}
	return created.ID, nil
}

// PracticeMockState reads the mock's state.json after it is paused; empty when
// the app stored nothing.
func (m *Manager) PracticeMockState(ctx context.Context, mockID string) ([]byte, error) {
	b, err := m.execTarOut(ctx, mockID, "/work/state.json", qualification.MaxFileBytes+1<<16)
	if err != nil {
		if errors.Is(err, errExecTarMissing) {
			return nil, nil
		}
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(b))
	h, err := tr.Next()
	if err != nil {
		return nil, err
	}
	if h.Typeflag != tar.TypeReg || h.Size > qualification.MaxFileBytes {
		return nil, errors.New("the mock's state is not a regular file within the limit")
	}
	return io.ReadAll(io.LimitReader(tr, h.Size))
}

// PausePractice freezes containers before evidence is read, so nothing in them
// can change what the checker sees.
func (m *Manager) PausePractice(ctx context.Context, ids ...string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := m.client.ContainerPause(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// UnpausePractice lets paused practice containers run again.
func (m *Manager) UnpausePractice(ctx context.Context, ids ...string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := m.client.ContainerUnpause(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// RemovePractice force-removes practice containers; one already gone is fine.
func (m *Manager) RemovePractice(ctx context.Context, ids ...string) error {
	var first error
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := m.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !client.IsErrNotFound(err) && first == nil {
			first = err
		}
	}
	return first
}

// RemovePracticeNetwork removes the practice network once nothing uses it.
func (m *Manager) RemovePracticeNetwork(ctx context.Context) error {
	m.leavePracticeNetwork(ctx)
	if err := m.client.NetworkRemove(ctx, PracticeNetworkName); err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

// CleanupPractice removes every container and network a practice run left
// behind (a restart mid-run); the twin of CleanupQualification.
func (m *Manager) CleanupPractice(ctx context.Context) error {
	list, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", PracticeLabel+"=true"))})
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := m.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !client.IsErrNotFound(err) {
			return err
		}
	}
	nets, err := m.client.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", PracticeLabel+"=true"))})
	if err != nil {
		return err
	}
	if len(nets) > 0 {
		m.leavePracticeNetwork(ctx)
	}
	for _, n := range nets {
		if err := m.client.NetworkRemove(ctx, n.ID); err != nil && !client.IsErrNotFound(err) {
			return err
		}
	}
	return nil
}

// PracticeAgentRequest sends one request to a practice desktop's agent by its
// address on the practice network.
func (m *Manager) PracticeAgentRequest(ctx context.Context, d PracticeDesktop, method, path string, body io.Reader) (*http.Response, error) {
	if net, err := netip.ParseAddr(d.IP); err != nil || !net.Is4() {
		return nil, qualification.ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, method, d.AgentURL()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	m.AuthorizeAgentRequest(req, d.Name)
	resp, err := agentClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrAgentUnreachable, err)
	}
	return resp, nil
}
