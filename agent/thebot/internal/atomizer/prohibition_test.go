package atomizer

import "testing"

func TestCodingProhibitionsDoNotBecomeBuildRequests(t *testing.T) {
	for _, request := range []string{
		"Process the bill in the GUI. Do not create invoices using HTTP/API, scripts, developer console or database. Save the draft.",
		"Fill out the purchase invoice. Don't write a Python script to enter it.",
		"Open the ERPNext browser and enter the bill. Never use SQL to create invoices.",
	} {
		if isCoding(request) {
			t.Errorf("GUI task classified as coding: %s", request)
		}
	}
	if !isCoding("Do not create invoices using scripts. Build a Python invoice validator.") {
		t.Error("an actual build request after a prohibition was lost")
	}
	if !describesSoftware("Build a Flask app. Do not use Docker.") {
		t.Error("positive software request was lost")
	}
}
