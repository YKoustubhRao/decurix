package desktop

import (
	"testing"
)

func TestAllPersonas(t *testing.T) {
	list := AllPersonas()
	if len(list) < 8 {
		t.Fatalf("expected at least 8 personas, got %d", len(list))
	}

	foundFinance := false
	for _, p := range list {
		if p.ID == "finance" {
			foundFinance = true
			if len(p.DefaultSkills) == 0 {
				t.Errorf("finance persona should have default skills")
			}
			if p.Specialization == "" {
				t.Errorf("finance persona should have specialization")
			}
		}
	}
	if !foundFinance {
		t.Errorf("expected to find finance persona")
	}
}

func TestGetPersona(t *testing.T) {
	p, ok := GetPersona("sales")
	if !ok {
		t.Fatalf("expected sales persona to exist")
	}
	if p.Department != "Sales" {
		t.Errorf("expected Department Sales, got %s", p.Department)
	}

	_, ok = GetPersona("nonexistent")
	if ok {
		t.Errorf("expected nonexistent persona to return false")
	}
}

func TestAllTeamsAndGetTeam(t *testing.T) {
	teams := AllTeams()
	if len(teams) < 3 {
		t.Fatalf("expected at least 3 teams, got %d", len(teams))
	}

	team, ok := GetTeam("business-core")
	if !ok {
		t.Fatalf("expected business-core team to exist")
	}
	if len(team.PersonaIDs) < 3 {
		t.Errorf("expected at least 3 personas in business-core, got %d", len(team.PersonaIDs))
	}

	_, ok = GetTeam("nonexistent-team")
	if ok {
		t.Errorf("expected nonexistent team to return false")
	}
}
