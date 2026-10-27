package catalog

import (
	"strings"
	"testing"
)

func TestDefaultCatalogIsValid(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.LlamaCppTag == "" || strings.Contains(c.Runtime.LlamaCppTag, "latest") {
		t.Fatalf("llama.cpp must be pinned, got %q", c.Runtime.LlamaCppTag)
	}
	for _, role := range []string{RoleBrain, RoleVision, RoleGround, RoleOCR, RoleASR, RoleVAD, RoleEmbed} {
		if len(c.ForRole(role)) == 0 {
			t.Errorf("no model for role %s", role)
		}
	}
	for _, m := range c.Models {
		for _, s := range m.Sources {
			if s.Runtime == RuntimeLlamaCpp && s.FileFor("model") == "" {
				t.Errorf("%s: a llama.cpp source has no model file", m.ID)
			}
			if _, vision := m.Roles[RoleVision]; vision && s.Runtime == RuntimeLlamaCpp && s.FileFor("mmproj") == "" {
				t.Errorf("%s: a vision model on llama.cpp needs its mmproj", m.ID)
			}
			if s.Kind == SourceGitHub && s.Unpack && len(s.Files) == 0 {
				t.Errorf("%s: an unpacked archive must say which files it gives", m.ID)
			}
		}
	}
}

func TestForRoleIsBestFirst(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	list := c.ForRole(RoleBrain)
	for i := 1; i < len(list); i++ {
		if list[i].Roles[RoleBrain] > list[i-1].Roles[RoleBrain] {
			t.Fatalf("%s ranked below %s", list[i-1].ID, list[i].ID)
		}
	}
}

func TestValidateRefusesBadEntries(t *testing.T) {
	bad := []string{
		`{"roles":[{"id":"brain"}],"models":[{"id":"x","roles":{"nope":1},"license_class":"permissive","sources":[{"kind":"ollama","runtime":"ollama","tag":"x","size_gb":1}]}]}`,
		`{"roles":[{"id":"brain"}],"models":[{"id":"x","roles":{"brain":1},"license_class":"maybe","sources":[{"kind":"ollama","runtime":"ollama","tag":"x","size_gb":1}]}]}`,
		`{"roles":[{"id":"brain"}],"models":[{"id":"x","roles":{"brain":1},"license_class":"permissive","sources":[{"kind":"hf","runtime":"llamacpp","size_gb":1}]}]}`,
		`{"roles":[{"id":"brain"}],"models":[{"id":"x","roles":{"brain":101},"license_class":"permissive","sources":[{"kind":"ollama","runtime":"ollama","tag":"x","size_gb":1}]}]}`,
	}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}
