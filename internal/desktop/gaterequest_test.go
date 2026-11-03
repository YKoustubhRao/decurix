package desktop

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// gateFixture is testdata/gate-signatures.json: inputs and the signatures the Python gate computes for them
// (gate_server.sign_request, sign_secrets). agent/tests verifies the same file with the gate's verifiers.
type gateFixture struct {
	Key     string `json:"key"`
	Request struct {
		TaskID string `json:"task_id"`
		Ts     string `json:"ts"`
		Nonce  string `json:"nonce"`
		Text   string `json:"text"`
		Sig    string `json:"sig"`
	} `json:"request"`
	Secrets struct {
		TaskID  string   `json:"task_id"`
		Ts      string   `json:"ts"`
		Nonce   string   `json:"nonce"`
		Secrets []string `json:"secrets"`
		Sig     string   `json:"sig"`
	} `json:"secrets"`
}

func readGateFixture(t *testing.T) gateFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gate-signatures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f gateFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The request and secrets signatures agree byte for byte with the gate's (the vectors are the Python side's), and
// each has its own label: a signed request is no signed reply or secrets list, nor the other way round.
func TestGateRequestAndSecretsSignaturesMatchTheGate(t *testing.T) {
	f := readGateFixture(t)
	r := f.Request
	if got := SignGateRequest(f.Key, r.TaskID, r.Ts, r.Nonce, r.Text); got != r.Sig {
		t.Fatalf("request signature = %s, the gate computes %s", got, r.Sig)
	}
	s := f.Secrets
	if got := SignGateSecrets(f.Key, s.TaskID, s.Ts, s.Nonce, s.Secrets); got != s.Sig {
		t.Fatalf("secrets signature = %s, the gate computes %s", got, s.Sig)
	}
	if SignGateRequest(f.Key, r.TaskID, r.Ts, r.Nonce, r.Text) == SignGateReply(f.Key, r.TaskID, r.Ts, r.Nonce, r.Text, "", "", "") {
		t.Fatal("a signed request reads as a signed reply")
	}
	if SignGateSecrets(f.Key, s.TaskID, s.Ts, s.Nonce, s.Secrets[:2]) == s.Sig ||
		SignGateSecrets(f.Key, s.TaskID, s.Ts, s.Nonce, []string{s.Secrets[1], s.Secrets[0], s.Secrets[2]}) == s.Sig {
		t.Fatal("another list of values has the same signature")
	}
}

// What reaches the gate's port is signed with that desktop's gate key, over exactly what is sent.
func TestTheGateGetsTheRequestAndTheSecretsSigned(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				NetworkName: {IPAddress: "10.0.0.5"},
			}},
		}, nil
	}
	gate := &agentStub{}
	m := newManagerWith(fake)
	m.HostGUISecret = []byte("secret")
	m.httpClient = &http.Client{Transport: gate}
	if err := m.SendGateRequest(context.Background(), "box", "t1", "sign in to records.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := m.SendGateSecrets(context.Background(), "box", "t1", []string{"Plum7Orbit"}); err != nil {
		t.Fatal(err)
	}
	if len(gate.bodies) != 2 {
		t.Fatalf("the gate got %d bodies", len(gate.bodies))
	}
	key := m.GateKey("box")
	r := gate.bodies[0]
	if r["sig"] != SignGateRequest(key, "t1", r["ts"].(string), r["nonce"].(string), "sign in to records.example.test") {
		t.Fatalf("request body %v is not signed over what it carries", r)
	}
	s := gate.bodies[1]
	if s["sig"] != SignGateSecrets(key, "t1", s["ts"].(string), s["nonce"].(string), []string{"Plum7Orbit"}) {
		t.Fatalf("secrets body %v is not signed over what it carries", s)
	}
	if r["nonce"] == s["nonce"] {
		t.Fatal("two bodies share a nonce")
	}
	if (&Manager{client: fake}).SendGateRequest(context.Background(), "box", "t1", "x") == nil {
		t.Fatal("a manager with no gate key sent a request unsigned")
	}
}

// Text that is not valid UTF-8 is signed as encoding/json sends it (each bad byte U+FFFD), so the gate, which hashes
// the string it decodes, verifies it: the vector is the Python gate's over that string (review CP-A N6).
func TestInvalidUTF8IsSignedAsItIsSent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "gate-signatures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Key  string `json:"key"`
		Case struct {
			TaskID string `json:"task_id"`
			Ts     string `json:"ts"`
			Nonce  string `json:"nonce"`
			RawHex string `json:"raw_hex"`
			Text   string `json:"text"`
			Sig    string `json:"sig"`
		} `json:"request_invalid_utf8"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	bad, _ := hex.DecodeString(f.Case.RawHex)
	text := GateText(string(bad))
	sent, _ := json.Marshal(string(bad))
	var decoded string
	_ = json.Unmarshal(sent, &decoded)
	if text != decoded || text != f.Case.Text {
		t.Fatalf("GateText %q, JSON sends %q, the fixture says %q", text, decoded, f.Case.Text)
	}
	if got := SignGateRequest(f.Key, f.Case.TaskID, f.Case.Ts, f.Case.Nonce, text); got != f.Case.Sig {
		t.Fatalf("signature %s, the gate computes %s", got, f.Case.Sig)
	}
}
