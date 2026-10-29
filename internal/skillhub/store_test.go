package skillhub

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// bundle builds an exported skill as a desktop writes one.
func bundle(t *testing.T, name string, fields map[string]any) []byte {
	t.Helper()

	skill := map[string]any{"name": name, "status": "ready", "steps": []any{map[string]any{"n": 1, "kind": "click"}}}
	for k, v := range fields {
		skill[k] = v
	}
	raw, err := json.Marshal(map[string]any{"format": BundleFormat, "skill": skill, "exported_at": 1})
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestPublishMakesANewVersionOnlyWhenTheSkillChanged(t *testing.T) {
	s := newStore(t)

	v1, created, err := s.Publish("pay-bill", bundle(t, "pay-bill", nil), "w1", "first")
	if err != nil || !created || v1.Version != 1 {
		t.Fatalf("first publish = %+v %v %v", v1, created, err)
	}

	// Same skill, different run count and export time: not a new version.
	again, created, err := s.Publish("pay-bill", bundle(t, "pay-bill", map[string]any{"runs": 9, "experience": []any{1}}), "w2", "")
	if err != nil || created || again.Version != 1 || again.PublishedFrom != "w1" {
		t.Fatalf("unchanged publish = %+v %v %v", again, created, err)
	}

	v2, created, err := s.Publish("pay-bill", bundle(t, "pay-bill", map[string]any{"title": "Pay a bill"}), "w2", "titled")
	if err != nil || !created || v2.Version != 2 || v2.SHA == v1.SHA {
		t.Fatalf("changed publish = %+v %v %v", v2, created, err)
	}

	got, err := s.Get("pay-bill", 0)
	if err != nil || got.Version != 2 || !strings.Contains(string(got.Bundle), "Pay a bill") {
		t.Fatalf("Get latest = %+v %v", got, err)
	}
	old, err := s.Get("pay-bill", 1)
	if err != nil || old.Version != 1 || old.Note != "first" {
		t.Fatalf("Get v1 = %+v %v", old, err)
	}
	if _, err := s.Get("pay-bill", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get v3 err = %v", err)
	}
	if _, err := s.Get("nope", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown err = %v", err)
	}
}

func TestBundleSHAIgnoresKeyOrder(t *testing.T) {
	a := []byte(`{"format":"decurix-skill/1","skill":{"name":"x","steps":[{"n":1,"kind":"click"}]}}`)
	b := []byte(`{"skill":{"steps":[{"kind":"click","n":1}],"name":"x"},"format":"decurix-skill/1","exported_at":5}`)

	sa, err := BundleSHA("x", a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := BundleSHA("x", b)
	if err != nil {
		t.Fatal(err)
	}
	if sa != sb {
		t.Fatalf("sha differs by key order: %s %s", sa, sb)
	}
}

func TestPublishRefusesBadInput(t *testing.T) {
	s := newStore(t)

	big := `{"format":"decurix-skill/1","skill":{"name":"x","pad":"` + strings.Repeat("a", MaxBundleBytes) + `"}}`
	cases := []struct {
		name, skill, from string
		bundle            []byte
	}{
		{"bad name", "Bad Name", "w1", bundle(t, "Bad Name", nil)},
		{"not json", "x", "w1", []byte("{")},
		{"wrong format", "x", "w1", []byte(`{"format":"other","skill":{"name":"x"}}`)},
		{"no skill", "x", "w1", []byte(`{"format":"decurix-skill/1"}`)},
		{"other skill", "x", "w1", bundle(t, "y", nil)},
		{"too big", "x", "w1", []byte(big)},
		{"bad worker", "x", "../w", bundle(t, "x", nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Publish(c.skill, c.bundle, c.from, ""); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestOutdatedReportsBehindAndMissing(t *testing.T) {
	s := newStore(t)

	mustPublish := func(name string, fields map[string]any) {
		if _, _, err := s.Publish(name, bundle(t, name, fields), "w0", ""); err != nil {
			t.Fatal(err)
		}
	}
	mustPublish("alpha", nil)
	mustPublish("alpha", map[string]any{"title": "v2"})
	mustPublish("beta", nil)
	mustPublish("gamma", nil)

	if err := s.RecordInstall("w1", "alpha", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInstall("w1", "gamma", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("beta", "clerk"); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("gamma", "clerk"); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("missing-skill", "clerk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assigning an unpublished skill: %v", err)
	}
	if _, err := s.SetWorkerRoles("w1", []string{"clerk", " clerk ", "auditor"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Outdated("w1")
	if err != nil {
		t.Fatal(err)
	}
	want := []Outdated{
		{Name: "alpha", Installed: 1, Latest: 2, Reason: "behind"},
		{Name: "beta", Installed: 0, Latest: 1, Reason: "missing", Role: "clerk"},
	}
	if len(got) != len(want) {
		t.Fatalf("Outdated = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Outdated[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A worker with no roles and nothing installed has nothing outdated.
	none, err := s.Outdated("w9")
	if err != nil || len(none) != 0 {
		t.Fatalf("Outdated(w9) = %+v %v", none, err)
	}

	roles, err := s.WorkerRoles("w1")
	if err != nil || strings.Join(roles, ",") != "auditor,clerk" {
		t.Fatalf("roles = %v %v", roles, err)
	}

	if err := s.Unassign("beta", "clerk"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Outdated("w1")
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("after unassign Outdated = %+v", got)
	}

	if err := s.ForgetWorker("w1"); err != nil {
		t.Fatal(err)
	}
	if roles, _ := s.WorkerRoles("w1"); len(roles) != 0 {
		t.Fatalf("roles after forget = %v", roles)
	}
}

func TestSetWorkerRolesValidates(t *testing.T) {
	s := newStore(t)

	cases := []struct {
		name   string
		worker string
		roles  []string
	}{
		{"bad worker", "", []string{"a"}},
		{"empty role", "w1", []string{""}},
		{"bad role", "w1", []string{"<script>"}},
		{"too many", "w1", strings.Split(strings.Repeat("r,", maxRoles+1), ",")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.SetWorkerRoles(c.worker, c.roles); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}

	if roles, err := s.SetWorkerRoles("w1", nil); err != nil || len(roles) != 0 {
		t.Fatalf("clearing roles = %v %v", roles, err)
	}
}

func TestListGroupsVersionsRolesAndInstalls(t *testing.T) {
	s := newStore(t)

	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %+v %v", list, err)
	}

	s.Publish("alpha", bundle(t, "alpha", nil), "w0", "")
	s.Publish("alpha", bundle(t, "alpha", map[string]any{"title": "t"}), "w0", "")
	s.Publish("beta", bundle(t, "beta", nil), "w0", "")
	s.RecordInstall("w1", "alpha", 1)
	s.Assign("alpha", "clerk")

	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v %v", list, err)
	}
	a := list[0]
	if a.Name != "alpha" || a.Latest != 2 || len(a.Versions) != 2 || a.Versions[0].Version != 2 ||
		len(a.Roles) != 1 || len(a.Installs) != 1 || a.Versions[0].Bundle != nil {
		t.Fatalf("alpha entry = %+v", a)
	}
	if list[1].Name != "beta" || len(list[1].Installs) != 0 {
		t.Fatalf("beta entry = %+v", list[1])
	}
}

// The schema is append-only: opening a store twice on one database applies
// nothing the second time.
func TestNewStoreTwiceOnOneDatabase(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
}
