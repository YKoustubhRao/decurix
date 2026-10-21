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

// Found live (2026-10-19, round 5, order-to-cash): the name of a messaging service in a file's name, or in the text
// a script writes into a spreadsheet, is not a message sent. These are the commands the worker was stopped on.
func TestAssessCommandIgnoresServiceNamesInFilesAndData(t *testing.T) {
	for _, cmd := range []string{
		`office read P2_orders_19Oct/HH-PO-7781.pdf && printf '\n---CHAT1---\n' && sed -n '1,220p' "P2_orders_19Oct/WhatsApp Chat with Arjun Rao (Maple Leaf Bakery).txt" && printf '\n---CHAT2---\n' && sed -n '1,220p' "P2_orders_19Oct/WhatsApp Chat with Daniel Shaw (Shaw Fitness Gym).txt"`,
		"python3 - <<'PY'\nimport json\nrows=[[\"Customer\",\"Source\"],[\"Maple Leaf Bakery\",\"WhatsApp chat: WhatsApp Chat with Arjun Rao (Maple Leaf Bakery).txt\"]]\nspec={\"sheets\":[{\"name\":\"Extracted Orders\",\"rows\":rows}]}\nprint(json.dumps(spec))\nPY > /tmp/extracted_orders_spec.json && office create P2_orders_19Oct/extracted_orders.xlsx - < /tmp/extracted_orders_spec.json && office read P2_orders_19Oct/extracted_orders.xlsx",
		"python3 - <<'PY'\nimport subprocess\nsrc='WhatsApp chat: x.txt'\nsubprocess.run(f\"office create 'a.xlsx' - < '{src}'\", shell=True, check=True)\nPY",
		`cat WhatsApp_export.txt | grep -i "telegram" > found.txt`,
		`grep -ril whatsapp ~/work/inbox`,
		`ls ~/work/inbox/WhatsApp*`,
		// A payment provider's name beside a word like invoice is a line of a statement, not a payment.
		`grep -i paypal statement.csv | grep -i invoice`,
		`awk -F, '/Stripe/ && /refund/ {print}' bank.csv`,
		"python3 - <<'PY'\nrows=[['PayPal transfer','invoice 1043 refund']]\nprint(rows)\nPY",
	} {
		if c := AssessCommand(cmd); c != nil {
			t.Errorf("AssessCommand(%q) = %q, want nil", cmd, c.Reason)
		}
	}
}

// A real send still needs a mechanism: a program, a library's send call, or a request to the service's endpoint.
func TestAssessCommandStillStopsRealMessagesAndPayments(t *testing.T) {
	for _, cmd := range []string{
		`twilio api:core:messages:create --to +911234567890 --from +10000000000 --body hi`,
		`echo hi | telegram-send --stdin`,
		`cd ~/work && slack-cli chat send "hello" #general`,
		`signal-cli -u +911234567890 send -m hi +919876543210`,
		"python3 - <<'PY'\nfrom twilio.rest import Client\nc = Client(sid, tok)\nc.messages.create(to='+91', from_='+1', body='hi')\nPY",
		"python3 - <<'PY'\nfrom slack_sdk import WebClient\nWebClient(t).chat_postMessage(channel='#a', text='x')\nPY",
		"python3 - <<'PY'\nimport telegram\nbot = telegram.Bot(t)\nbot.send_message(chat_id=1, text='x')\nPY",
		"python3 - <<'PY'\nimport pywhatkit\npywhatkit.sendwhatmsg('+91', 'hi', 10, 5)\nPY",
		"python3 - <<'PY'\nimport requests\nrequests.post('https://hooks.slack.com/services/T/B/X', json={'text': 'hi'})\nPY",
		"python3 - <<'PY'\nimport requests\nrequests.post('https://api.telegram.org/bot123/sendMessage', data={'text': 'hi'})\nPY",
		`stripe charges create --amount 2000 --currency usd --source tok_visa`,
		`stripe refunds create --charge ch_123`,
		"python3 - <<'PY'\nimport stripe\nstripe.PaymentIntent.create(amount=2000, currency='usd')\nPY",
		"python3 - <<'PY'\nimport requests\nrequests.post('https://api.stripe.com/v1/payouts', data={'amount': 1})\nPY",
		"python3 - <<'PY'\nimport razorpay\nclient = razorpay.Client(auth=(a, b))\nclient.refund.create({'payment_id': 'p'})\nPY",
	} {
		if AssessCommand(cmd) == nil {
			t.Errorf("AssessCommand(%q) = nil, want a concern", cmd)
		}
	}
}

// "I'll send them myself" is the opposite of a request to send; and a stage of a planned request starts with its own
// words, so no request rule reads the rest of its text as an instruction to send.
func TestAssessRequestIgnoresDraftsThePersonSendsThemselves(t *testing.T) {
	for _, prompt := range []string{
		"Enter every approvable order in the portal and write customer_replies.txt: the reply for each customer, naming the items and totals. Leave them as drafts, I'll send them myself.",
		"Stage 5 of 5 of a longer request a person made: Plans and replies.\nDo only this stage now.\n\nThis stage: write the reply for each customer (I'll send them myself; never send a message).",
		"Draft a reply to each customer; do not send anything - I send the messages.",
	} {
		if c := AssessRequest(prompt); c != nil {
			t.Errorf("AssessRequest(%.80q) = %q, want nil", prompt, c.Reason)
		}
	}
}
