package config

import (
	"fmt"
	"strings"
)

// Error is a single configuration problem: field path, reason and action.
type Error struct {
	Path   string
	Reason string
	Action string
}

func (e Error) Error() string {
	s := e.Path + ": " + e.Reason
	if e.Action != "" {
		s += "；" + e.Action
	}
	return s
}

// Errors collects every problem found so the operator sees them all at once.
type Errors []Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i := range e {
		parts[i] = e[i].Error()
	}
	return strings.Join(parts, "\n")
}

func (e *Errors) add(path, reason, action string) {
	*e = append(*e, Error{Path: path, Reason: reason, Action: action})
}

func (e *Errors) addf(path, action, format string, args ...any) {
	e.add(path, fmt.Sprintf(format, args...), action)
}

// OrNil returns nil when no problems were collected.
func (e Errors) OrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}
