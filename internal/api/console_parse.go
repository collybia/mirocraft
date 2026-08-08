package api

import (
	"errors"
	"fmt"
	"regexp"
)

// playerJoinLeave matches the lines a server writes when someone connects or
// disconnects.
//
// Parsing the log is not how anyone would choose to learn this, and it is
// worth being honest about why it is here: the Minecraft protocol offers no
// notification, and the status ping gives only a capped sample of names. A
// plugin could report it properly, but requiring one to see who joined would
// be a poor trade for an operator running vanilla.
//
// The pattern is therefore deliberately loose about the prefix — the
// timestamp and thread differ between vanilla, Paper and forks — and strict
// about the name, which must look like a Mojang name to be reported at all.
// A chat message reading "Steve joined the game" cannot match, because chat
// lines carry the speaker in angle brackets before the text.
var playerJoinLeave = regexp.MustCompile(
	`^\[[^\]]*\]:? *(?:\[[^\]]*\]:? *)?([A-Za-z0-9_]{3,16}) (joined|left) the game\s*$`)

// parsePlayerLine reports the player named in a join or leave line.
func parsePlayerLine(text string) (name string, joined bool, ok bool) {
	match := playerJoinLeave.FindStringSubmatch(text)
	if match == nil {
		return "", false, false
	}
	return match[1], match[2] == "joined", true
}

// Errors used by the webhook validation, kept together so their wording stays
// consistent.
var (
	errNoURL     = errors.New("a url is required")
	errBadURL    = errors.New("the url is not valid")
	errBadScheme = errors.New("the url must be http or https")
)

func errUnknownEvent(t string) error {
	return fmt.Errorf("unknown event type %q; subscribing to one would deliver nothing", t)
}
