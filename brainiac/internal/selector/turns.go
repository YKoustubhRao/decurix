// What a turn costs on this machine: the selector reads the speeds
// `brainiac bench` measured (speed.json) and, where nothing was measured,
// estimates them. A desktop's turn is mostly prompt to read, so a model is
// judged by its seconds per turn, cached and cold, not by tokens written
// per second alone.
package selector

import (
	"fmt"
	"sort"

	"brainiac/internal/catalog"
	"brainiac/internal/speed"
)

// A turn as the plan reckons it: WarmTurnTokens of new prompt (the start
// came from the prompt cache) or ColdTurnTokens read from the start, and an
// answer of TurnAnswerTokens.
const (
	WarmTurnTokens   = 1000
	ColdTurnTokens   = 6000
	TurnAnswerTokens = 200
)

// prefillGFLOPSPerCore is a CPU core's useful work reading a quantized
// prompt, for an estimate before anything is measured. Reading a prompt
// costs about 2 floating-point operations per weight per token. It is set
// low: an estimate that flatters a slow machine is worse than one that
// undersells it.
const prefillGFLOPSPerCore = 25

// measured is the bench's result for a candidate, when it is current: the
// same file, and the same placement.
func (s *selection) measured(c candidate) (speed.Result, bool) {
	if s.speeds == nil || c.src.Runtime != catalog.RuntimeLlamaCpp || !c.installed || s.st == nil {
		return speed.Result{}, false
	}
	r, ok := s.speeds.Find(c.model.ID, c.src.Label(), speed.FileSig(s.st.SourceFiles(c.src)["model"]))
	if !ok || r.OnGPU != c.onGPU || r.DecodeTPS <= 0 || r.PrefillShortTPS <= 0 {
		return speed.Result{}, false
	}
	return r, true
}

// activeParamsB is the parameters used per token, in billions.
func activeParamsB(m catalog.Model) float64 {
	if m.ActiveB > 0 {
		return m.ActiveB
	}
	return m.ParamsB
}

// calibrateDecode replaces the decode estimate with what models measured on
// this machine's CPU: their tokens per second times the weights each token
// reads. The median of them, so one odd run does not set it.
func (s *selection) calibrateDecode() {
	if s.pol.DecodeGBps > 0 || s.speeds == nil {
		return
	}
	var gbps []float64
	var names []string
	for _, r := range s.speeds.Results {
		m, ok := s.cat.Model(r.Model)
		if !ok || r.OnGPU || r.DecodeTPS <= 0 {
			continue
		}
		for _, src := range m.Sources {
			if src.Label() == r.Source {
				if act := activeGB(m, src); act > 0 {
					gbps = append(gbps, r.DecodeTPS*act)
					names = append(names, fmt.Sprintf("%s %.1f tok/s", r.Model, r.DecodeTPS))
				}
			}
		}
	}
	if len(gbps) == 0 {
		return
	}
	sort.Float64s(gbps)
	s.plan.DecodeGBps = round2(gbps[len(gbps)/2])
	s.plan.DecodeBasis = "measured here: " + joinMax(names, 3)
}

func joinMax(a []string, n int) string {
	if len(a) > n {
		a = append(a[:n:n], fmt.Sprintf("%d more", len(a)-n))
	}
	out := ""
	for i, x := range a {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// prefillEstimate is prompt tokens read per second, when nothing measured
// this model: scaled from another model measured on this CPU by the work per
// token, else from the cores.
func (s *selection) prefillEstimate(c candidate) float64 {
	act := activeParamsB(c.model)
	if act <= 0 {
		return 0
	}
	if s.speeds != nil && !c.onGPU {
		for _, r := range s.speeds.Results {
			if m, ok := s.cat.Model(r.Model); ok && !r.OnGPU && r.PrefillShortTPS > 0 && activeParamsB(m) > 0 {
				return round1(r.PrefillShortTPS * activeParamsB(m) / act)
			}
		}
	}
	cores := float64(max(1, s.cs.CPU.PhysicalCores))
	if c.onGPU {
		cores *= 40 // a GPU reads prompts far faster than a CPU; a loose floor
	}
	return round1(cores * prefillGFLOPSPerCore / (2 * act))
}

// turnTimes fills a chat pick's speeds and seconds per turn, measured when
// the bench has run, estimated otherwise.
func (s *selection) turnTimes(p *Pick, c candidate) {
	if p.SpeedUnit != "tok/s" {
		return
	}
	if r, ok := s.measured(c); ok {
		p.Speed, p.PrefillTPS, p.SpeedBasis = r.DecodeTPS, r.PrefillShortTPS, "measured"
		p.WarmTurnSec = round1(r.TurnSec(WarmTurnTokens, TurnAnswerTokens))
		p.ColdTurnSec = round1(r.TurnSec(ColdTurnTokens, TurnAnswerTokens))
		return
	}
	pre := s.prefillEstimate(c)
	if pre <= 0 || p.Speed <= 0 {
		return
	}
	p.PrefillTPS, p.SpeedBasis = pre, "estimated"
	p.WarmTurnSec = round1(float64(WarmTurnTokens)/pre + float64(TurnAnswerTokens)/p.Speed)
	// A long prompt reads somewhat slower: attention grows with the context.
	p.ColdTurnSec = round1(float64(ColdTurnTokens)/(pre*0.8) + float64(TurnAnswerTokens)/p.Speed)
}
