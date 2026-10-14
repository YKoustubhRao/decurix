package safety

import (
	"testing"
)

func TestAssessCommandStopsConsequentialCommands(t *testing.T) {
	for _, cmd := range []string{
		`echo "rent is late" | mail -s rent landlord@example.com`,
		`sendmail boss@example.com < note.txt`,
		`git push origin main`,
		`cd repo && git push`,
		`npm publish`,
		`docker push me/app:latest`,
		`gh pr create --fill`,
		`rm -rf /`,
		`rm -rf ~`,
		`rm -rf "$HOME"`,
		`sudo rm -r /home/headless`,
		`dd if=/dev/zero of=/dev/sda bs=1M`,
		`sudo shutdown -h now`,
		`reboot`,
		`curl -X POST https://api.example.com/orders -d '{"qty":1}'`,
		`curl --data "msg=hi" https://hooks.slack.com/services/x`,
		`wget --post-data "a=1" https://example.com/form`,
		`chmod -R 777 /`,
		`echo "$(mail -s hi boss@example.com < note.txt)"`,
		"x=\x60mail -s hi boss@example.com < note.txt\x60",
		`sh -c 'cat f | mail -s x a@b.example'`,
		"cat <<EOF | mail -s x a@b.example\nhello\nEOF",
		"bash <<'SH'\nmail -s x a@b.example < f\nSH",
		"cd ~/work\nmail -s report boss@example.com < r.txt",
		"python3 - <<'PY'\nimport smtplib\ns = smtplib.SMTP('mx')\ns.send_message(msg)\nPY",
	} {
		if AssessCommand(cmd) == nil {
			t.Errorf("AssessCommand(%q) = nil, want a concern", cmd)
		}
	}
}

// The gate is only worth having if ordinary work sails through it: a human
// asked to approve "ls" learns to approve everything without reading.
func TestAssessCommandLetsOrdinaryWorkThrough(t *testing.T) {

	for _, cmd := range []string{
		`ls -la /etc`,
		`uname -r`,
		`df -h /`,
		`rm -rf /tmp/build`,
		`rm -rf ~/work/node_modules`,
		`rm -r /home/headless/work/old`,
		`mkdir -p ~/work/notes && echo hi > ~/work/notes/a.txt`,
		`sudo apt-get install -y jq`,
		`pip install --user requests`,
		`git status && git commit -am wip`,
		`curl -s https://example.com`,
		`curl -X POST http://localhost:8000/items -d '{"a":1}'`,
		`curl -d @body.json 127.0.0.1:5050/api`,
		`grep -r "mail" .`,
		`python3 -c "print('shutdown sequence')"`,
		`grep -E 'outbox|mail' notes.txt`,
		`echo "$(date) mail checked" >> log.txt`,
		// Found live (2026-10-14): a variable named mail in a Python here-document stopped a price check for an
		// approval "because it sends email".
		"python3 - <<'PY'\nout='/w/a.xlsx'\nmail='/w/flagged.txt'\nopen(mail, 'w').write('x')\nPY",
		"python3 - <<PY\nmail = compose()\nshutdown = False\nPY\necho done",
	} {
		if c := AssessCommand(cmd); c != nil {
			t.Errorf("AssessCommand(%q) = %q, want nil", cmd, c.Reason)
		}
	}
}

func TestAssessRequestStopsDirectConsequentialInstructions(t *testing.T) {
	for _, prompt := range []string{
		"pay the 400 dollar invoice",
		"please pay my electricity bill",
		"email my landlord that the rent will be late",
		"send an email to hr@example.com saying I quit",
		"can you send a message to the team channel",
		"text my mom happy birthday",
		"buy me a new laptop charger",
		"book me a flight to Delhi on Friday",
		"post this summary on LinkedIn",
		"cancel my Netflix subscription",
	} {
		if AssessRequest(prompt) == nil {
			t.Errorf("AssessRequest(%q) = nil, want a concern", prompt)
		}
	}
}

func TestAssessRequestIgnoresQuestionsAndBuilds(t *testing.T) {
	for _, prompt := range []string{
		"how do I send an email from python?",
		"write a script that emails me a daily report",
		"build a payment page with stripe checkout",
		"what's the text formatting syntax in markdown?",
		"order the list in notes.txt alphabetically",
		"summarise the invoice in ~/work/invoice.pdf",
		"hello, whats your name?",
		"check the disk space and report back",
	} {
		if c := AssessRequest(prompt); c != nil {
			t.Errorf("AssessRequest(%q) = %q, want nil", prompt, c.Reason)
		}
	}
}

func TestIsApproval(t *testing.T) {
	for _, reply := range []string{"yes", "Yes, go ahead", "ok", "approved", "go for it", "sure, do it", "LGTM"} {
		if !IsApproval(reply) {
			t.Errorf("IsApproval(%q) = false, want true", reply)
		}
	}
	for _, reply := range []string{"no", "don't", "yes but don't push yet", "wait", "use rsync instead", "", "what would it do?"} {
		if IsApproval(reply) {
			t.Errorf("IsApproval(%q) = true, want false", reply)
		}
	}
}
