package selector

import (
	"fmt"
	"math"
	"strings"

	"brainiac/internal/catalog"
)

// minutes says a duration in words a person reads at a glance.
func minutes(sec float64) string {
	if sec < 90 {
		return fmt.Sprintf("%.0f s", sec)
	}
	return fmt.Sprintf("%.0f min", math.Round(sec/60))
}

// chooseDomains looks, field by field, for a specialist that beats the chosen
// brain there by DomainMargin and fits this machine. A field with no such
// specialist is answered by the brain, and the plan says why. Specialists are
// loaded only when a request in their field comes, so they are not counted as
// staying in memory beside the brain; on a small machine they swap with it.
func (s *selection) chooseDomains() {
	wanted := map[string]bool{}
	for _, d := range s.pol.Domains {
		wanted[strings.TrimSpace(d)] = true
	}
	if wanted["none"] {
		return
	}
	brainPick, hasBrain := s.plan.Pick(catalog.RoleBrain)
	var brain catalog.Model
	if hasBrain {
		brain, _ = s.cat.Model(brainPick.Model)
	}
	for _, role := range s.cat.Domains() {
		if len(wanted) > 0 && !wanted[role.ID] {
			continue
		}
		brainQ := 0.0
		if hasBrain {
			brainQ = brain.DomainQuality(role.ID)
		}
		var best *candidate
		bestScore := -1.0
		var rejected []Reject
		consider := func(c candidate) {
			score := c.model.Roles[role.ID]
			if c.installed {
				score += 2 // a tie goes to what needs no download
			}
			if best == nil || score > bestScore || (score == bestScore && c.key() < best.key()) {
				cc := c
				best, bestScore = &cc, score
			}
		}
		for _, m := range s.cat.ForRole(role.ID) {
			if _, general := m.Roles[catalog.RoleBrain]; general {
				// A general model's field score is there to compare the
				// brain with; a specialist is a model made for the field.
				continue
			}
			q := m.Roles[role.ID]
			if q < role.MinQuality {
				rejected = append(rejected, Reject{Model: m.ID, Why: fmt.Sprintf("quality %.0f is under the %.0f floor", q, role.MinQuality)})
				continue
			}
			if hasBrain && q < brainQ+s.pol.DomainMargin {
				rejected = append(rejected, Reject{Model: m.ID, Why: fmt.Sprintf("quality %.0f does not beat the brain's %.0f here by %.0f", q, brainQ, s.pol.DomainMargin)})
				continue
			}
			if c, ok := s.chosen[m.ID]; ok {
				consider(c)
				continue
			}
			for _, src := range m.Sources {
				c, why := s.evaluate(m, src)
				if why == "" && c.download > s.diskLeft {
					why = fmt.Sprintf("download %.1f GB is over the %.1f GB of disk left to use", c.download, s.diskLeft)
				}
				if why == "" {
					if sp := s.speedFor(c, role); !fast(sp, role) {
						why = "too slow here: " + unitText(sp, role.SpeedUnit)
					}
				}
				if why != "" {
					rejected = append(rejected, Reject{Model: m.ID, Source: src.Label(), Why: why})
					continue
				}
				consider(c)
			}
		}
		if best == nil {
			why := "no specialist in the catalog"
			if len(rejected) > 0 {
				why = "no specialist fits this machine and beats the brain here (" + rejected[0].Model + ": " + rejected[0].Why + ")"
			}
			if role.Note != "" {
				why += ". " + role.Note
			}
			s.plan.ByBrain = append(s.plan.ByBrain, Missing{Role: role.ID, Why: why, Rejected: dedupe(rejected)})
			continue
		}
		why := fmt.Sprintf("beats the brain's %.0f here with %.0f", brainQ, best.model.Roles[role.ID])
		if !hasBrain {
			why = "the best that fits (there is no local brain)"
		}
		p := s.pickFrom(*best, role, why)
		b := s.budgets[best.pool]
		p.Swaps = b == nil || s.used[best.pool]+best.memGB > b.BudgetGB
		if _, reused := s.chosen[best.model.ID]; reused {
			p.Download = 0
			p.Why += "; already chosen for another field"
		} else {
			s.chosen[best.model.ID] = *best
			s.diskLeft -= best.download
		}
		if best.model.Thinks {
			p.Why += "; a reasoning model: it thinks before answering"
			if p.Speed > 0 && best.model.ThinkTokens > 0 {
				p.AnswerSec = round1(float64(best.model.ThinkTokens+300) / p.Speed)
				p.Why += fmt.Sprintf(", about %s for a hard problem here", minutes(p.AnswerSec))
			}
		}
		p.Rejected = dedupe(rejected)
		s.plan.Picks = append(s.plan.Picks, p)
	}
}
