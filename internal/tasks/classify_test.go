package tasks

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"what is the current kernel version?":              TypeSystem,
		"hello, whats your desktop name?":                  TypeQuestion,
		"how many files are in /etc":                       TypeFiles,
		"write a poem to koustubh.txt":                     TypeFiles,
		"pay the 400 dollar invoice":                       TypeConsequential,
		"email my landlord that the rent is late":          TypeConsequential,
		"In the admin panel, add Dana Scott as an Editor":  TypeScreen,
		"fill the LibreOffice form with the order details": TypeScreen,
		"what is the newest Go version? check the website": TypeWeb,
		"write a python script that dedupes lines":         TypeCode,
		"install ripgrep":                                  TypeSystem,
		"tidy up":                                          TypeOther,
		"ping - just reply pong":                           TypeQuestion,
		"click Send newsletter in the admin panel":         TypeConsequential,
	}

	for request, want := range cases {
		if got := Classify(request); got != want {
			t.Errorf("Classify(%q) = %s, want %s", request, got, want)
		}
	}
}

func TestInterventionReason(t *testing.T) {
	cases := []struct {
		kind, text, want string
	}{
		{InterventionApproval, "Approval needed before I click button \"Pay now INV-1042\": looks like a payment. Nothing was clicked or sent.", ReasonApprovalPayment},
		{InterventionApproval, "Approval needed before I press Enter in a messaging page", ReasonApprovalMessage},
		{InterventionApproval, "Approval needed before I delete the production database", ReasonApprovalOther},
		{InterventionApproval, "Approval needed before I click button \"Delete account\": looks consequential. Nothing was clicked or sent. Reply yes to let me do it, or tell me what to do instead.", ReasonApprovalOther},
		// The Ollama loop's HELP is also its safety brake.
		{InterventionHelp, "Should I go ahead and send this email to the landlord?", ReasonApprovalMessage},
		{InterventionHelp, "The reasoning model (qwen2.5-coder:3b) at http://host.docker.internal:11434 accepted the request but did not finish answering in time.", ReasonBackend},
		{InterventionHelp, "I need the admin password to sign in.", ReasonCredentials},
		// Seen live from qwen2.5-coder:3b on "pay the 400 dollar invoice".
		{InterventionHelp, "You need to provide your payment method or secret to proceed.", ReasonCredentials},
		{InterventionHelp, "mkdir: cannot create directory '/opt/x': Permission denied", ReasonPermission},
		{InterventionHelp, "bash: rg: command not found", ReasonMissingTool},
		{InterventionHelp, "gui: nothing on the screen matches button \"Sign in\".", ReasonScreen},
		{InterventionHelp, "The agent ran its full budget of 50 steps without finishing the task", ReasonLoop},
		{InterventionHelp, "Which spreadsheet do you mean?", ReasonClarification},
		{InterventionHelp, "Something odd happened.", ReasonOtherHelp},
	}

	for _, c := range cases {
		if got := InterventionReason(c.kind, c.text); got != c.want {
			t.Errorf("InterventionReason(%s, %q) = %s, want %s", c.kind, c.text, got, c.want)
		}
	}
}

func TestValidID(t *testing.T) {
	if !ValidID(newID("t")) {
		t.Fatal("an issued id is not valid")
	}

	for _, id := range []string{"", "t123", "../../etc/passwd", "tZZZZZZZZZZZZZZZZ"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}
