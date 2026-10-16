package httpapi

import "testing"

func TestAVisionModelsControlsAreKeptOnlyWhenWellFormed(t *testing.T) {
	answer := "Here they are:\n```json\n" + `{"nodes":[
		{"role":"button","name":"Save","box":[10,10,80,30]},
		{"role":"textbox","name":"","box":[10,50,200,30],"value":"Acme"},
		{"role":"textbox","name":"Password","box":[10,90,200,30],"password":true,"value":"hunter2"},
		{"role":"Button","name":"  Print  ","box":[900,10,80,30],"focused":true},
		{"role":"spaceship","name":"Launch","box":[10,130,80,30]},
		{"role":"button","name":"","box":[10,170,80,30]},
		{"role":"button","name":"Outside","box":[2000,10,80,30]},
		{"role":"link","name":"Tiny","box":[10,10,1,1]}]}` + "\n```"
	nodes, err := parseVisionControls(answer, 960, 600)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range nodes {
		names = append(names, n.Role+":"+n.Name)
	}
	want := []string{"button:Save", "textbox:", "textbox:Password", "button:Print"}
	if len(names) != len(want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("kept %v, want %v", names, want)
		}
	}
	if nodes[2].Value != "" || !nodes[2].Password {
		t.Errorf("a password field kept its value: %+v", nodes[2])
	}
	if nodes[1].Value != "Acme" {
		t.Errorf("a field lost its value: %+v", nodes[1])
	}
	// A box that runs past the picture is cut to it; a model never says what has the keyboard.
	if nodes[3].Box[2] != 60 || nodes[3].Focused {
		t.Errorf("Print: %+v", nodes[3])
	}
}

func TestAVisionAnswerThatIsNotJSONIsAnError(t *testing.T) {
	if _, err := parseVisionControls("I cannot see any controls.", 100, 100); err == nil {
		t.Error("prose was read as controls")
	}
}

// The field the picture shows as active is said in a line of text over it, never as Windows' "focused".
func TestAVisionReadSaysWhichFieldHasTheCursor(t *testing.T) {
	answer := `{"cursor":"Under","nodes":[
		{"role":"textbox","name":"Name","box":[10,10,200,20],"value":"Kaveri Electricals"},
		{"role":"textbox","name":"Under","box":[10,40,200,20],"value":"Capital Account"}]}`
	nodes, err := parseVisionControls(answer, 960, 600)
	if err != nil {
		t.Fatal(err)
	}
	last := nodes[len(nodes)-1]
	if last.Role != "text" || last.Name != "the cursor is in: Under" || last.Box != nodes[1].Box || last.Focused {
		t.Fatalf("cursor line = %+v", last)
	}
	for _, n := range nodes {
		if n.Focused {
			t.Fatalf("%+v was marked focused", n)
		}
	}
	// A cursor naming no field read is not said.
	nodes, _ = parseVisionControls(`{"cursor":"Nowhere","nodes":[{"role":"textbox","name":"Name","box":[1,1,50,20]}]}`, 960, 600)
	if len(nodes) != 1 {
		t.Fatalf("a cursor on no field was kept: %+v", nodes)
	}
}
