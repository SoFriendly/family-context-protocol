package ui

import "fmt"

// PasteMessage is what a person pastes into their AI assistant to set it up:
// plain words saying what to do, then the one-time link. from names whose
// assistant they'll be coordinating with ("" for the first setup).
func PasteMessage(link, from string) string {
	with := "my partner's AI assistant"
	if from != "" {
		with = from
	}
	return fmt.Sprintf("Please connect to our family's FCP message bus so you can share updates and coordinate with %s. "+
		"Open this link and follow the instructions there. It works once:\n%s", with, link)
}
