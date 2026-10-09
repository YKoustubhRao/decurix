package winhost

import (
	"encoding/base64"
	"testing"
)

func treeLine(fresh, body string) string {
	return "TREE " + fresh + " " + base64.StdEncoding.EncodeToString([]byte(body))
}

func TestTheHelpersControlsAreRead(t *testing.T) {
	c, err := parseControls(treeLine("1", `{"w":556,"h":713,"title":"Character Map","nodes":[
		{"role":"textbox","name":"Password","box":[1,2,30,10],"password":true},
		{"role":"checkbox","name":"Advanced view","box":[13,472,224,24],"checked":false},
		{"role":"button","name":"Close","box":[496,1,60,37]},
		{"role":"text","name":"Font :","box":[13,52,61,24]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Fresh || c.W != 556 || c.Title != "Character Map" || len(c.Nodes) != 4 || c.Source != "uia" {
		t.Fatalf("read %+v", c)
	}
	if !c.Nodes[0].Password || c.Nodes[1].Checked == nil || *c.Nodes[1].Checked {
		t.Errorf("states lost: %+v", c.Nodes[:2])
	}
	// The title bar's Close and a label are not controls of the app.
	if n := c.Actionable(); n != 2 {
		t.Errorf("Actionable = %d, want 2", n)
	}
}

func TestAReadThatHasNotFinishedIsNeverFresh(t *testing.T) {
	c, err := parseControls(treeLine("1", `{"w":0,"h":0,"nodes":[],"pending":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Fresh || !c.Pending {
		t.Errorf("a pending read: %+v", c)
	}
}

func TestAnAppWithOnlyItsTitleBarHasNothingToUse(t *testing.T) {
	c, _ := parseControls(treeLine("0", `{"w":10,"h":10,"nodes":[{"role":"button","name":"Minimize","box":[0,0,5,5]},{"role":"button","name":"Close","box":[5,0,5,5]},{"role":"text","name":"Login","box":[0,0,9,3]}]}`))
	if c.Actionable() != 0 {
		t.Errorf("a window showing only its title bar counted %d controls", c.Actionable())
	}
}

func TestAnAnswerThatIsNotControlsIsAnError(t *testing.T) {
	for _, resp := range []string{"ERR no app", "TREE 1", "TREE 1 !!!", "TREE 1 " + base64.StdEncoding.EncodeToString([]byte("not json"))} {
		if _, err := parseControls(resp); err == nil {
			t.Errorf("%q was read as controls", resp)
		}
	}
}
