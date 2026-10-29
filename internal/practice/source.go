package practice

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// SourceFacts are what the worker's agent reads off a skill or recording for
// classifying it (agent/decurix_agent/practice.py); no bytes, no secrets.
type SourceFacts struct {
	Hosts         []string `json:"hosts"`
	Schemes       []string `json:"schemes"`
	Consequential bool     `json:"consequential"`
	Manual        bool     `json:"manual"`
	HostApp       bool     `json:"host_app"`
	Grade         string   `json:"grade"`
	Steps         int      `json:"steps"`
}

// AgentSource is one source as the agent exports it at
// GET /skills/<name>/practice-source or /recordings/<id>/practice-source.
// Files are exact saved bytes keyed by path under the desktop's $HOME.
type AgentSource struct {
	Kind  string            `json:"kind"`
	Name  string            `json:"name"`
	Title string            `json:"title"`
	Brief string            `json:"brief"`
	Facts SourceFacts       `json:"facts"`
	Files map[string][]byte `json:"files,omitempty"`
}

// SourceRow is one line of the page's pick list: a source, its kind of work,
// its lint grade and why it cannot be practised, if it cannot.
type SourceRow struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Title      string `json:"title"`
	WorkKind   string `json:"work_kind"`
	OutOfScope string `json:"out_of_scope,omitempty"`
	Grade      string `json:"grade"`
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)

// Classify decides a source's kind of work and, when it cannot be practised in
// the MVP, the reason. Generic: it reads only the shape of the work.
func Classify(f SourceFacts) (workKind, outOfScope string) {
	hosts := uniqueHosts(f.Hosts)
	switch {
	case f.Steps == 0:
		return qualification.WorkOffice, "nothing recorded: the source has no steps"
	case f.Manual:
		return qualification.WorkOffice, "private step: a step a person did by hand, such as a sign-in"
	case f.HostApp:
		return qualification.WorkOffice, "host app: a program that runs on the person's own computer"
	case len(hosts) > 1:
		return qualification.WorkWeb, "live site: the work spans more than one web site"
	case len(hosts) == 0:
		return qualification.WorkOffice, ""
	}
	host := hosts[0]
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return qualification.WorkWeb, "local address: the practice browser cannot be pointed away from an address or localhost"
	}
	if !hostName.MatchString(host) {
		return qualification.WorkWeb, "live site: the recorded address is not a plain host name"
	}
	for _, s := range f.Schemes {
		if strings.EqualFold(s, "https") {
			return qualification.WorkWeb, "secure site: the mock app cannot serve https pages yet"
		}
	}
	return qualification.WorkWeb, ""
}

func uniqueHosts(hosts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// Row is the source's line in the pick list.
func (a AgentSource) Row() SourceRow {
	kind, out := Classify(a.Facts)
	return SourceRow{Kind: a.Kind, Name: a.Name, Title: a.Title, WorkKind: kind, OutOfScope: out, Grade: a.Facts.Grade}
}

// PracticeSource is the source as a suite pins it.
func (a AgentSource) PracticeSource() qualification.PracticeSource {
	kind, out := Classify(a.Facts)
	src := qualification.PracticeSource{Kind: a.Kind, Name: a.Name, Title: a.Title, WorkKind: kind, OutOfScope: out, Brief: a.Brief,
		Consequential: a.Facts.Consequential, Files: a.Files}
	if src.Files == nil {
		src.Files = map[string][]byte{}
	}
	if kind == qualification.WorkWeb {
		src.Hosts = uniqueHosts(a.Facts.Hosts)
		src.Scheme = "http"
	}
	src.SHA256 = qualification.SourceFingerprint(src)
	return src
}
