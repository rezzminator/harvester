// Package engine names the engines accepted by harvester ask.
package engine

import (
	"fmt"
	"strings"
)

type ID string

const (
	Claude   ID = "cc"
	Codex    ID = "cx"
	OpenCode ID = "ox"
)

type Descriptor struct {
	ID                      ID
	Short, LongName, Binary string
}

var descriptors = []Descriptor{{Claude, "Claude", "claude", "claude"}, {Codex, "Codex", "codex", "codex"}, {OpenCode, "OpenCode", "opencode", "opencode"}}

func Lookup(id ID) (Descriptor, error) {
	for _, d := range descriptors {
		if d.ID == id {
			return d, nil
		}
	}
	return Descriptor{}, fmt.Errorf("unknown engine %q (want cc/claude, cx/codex, ox/opencode)", id)
}
func MustLookup(id ID) Descriptor {
	d, err := Lookup(id)
	if err != nil {
		panic(err)
	}
	return d
}
func Parse(value string) (ID, error) {
	wanted := strings.ToLower(strings.TrimSpace(value))
	for _, d := range descriptors {
		if wanted == string(d.ID) || wanted == d.LongName {
			return d.ID, nil
		}
	}
	if wanted == "" {
		return "", fmt.Errorf("no engine given (want cc/claude, cx/codex, ox/opencode)")
	}
	return "", fmt.Errorf("unknown engine %q (want cc/claude, cx/codex, ox/opencode)", value)
}
