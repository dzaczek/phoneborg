package controller

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// RouterSettings are the semantic router's settings (ADR-033), part of
// GET /admin/gateway and stored in the routing file.
type RouterSettings struct {
	Enabled    bool                  `json:"enabled"`
	Classifier string                `json:"classifier"`
	Timeout    string                `json:"timeout"`
	Classes    []RouterClassSettings `json:"classes"`
}

// RouterClassSettings is one class; Letter, the classifier's answer for it,
// follows from the class's position and is ignored on input.
type RouterClassSettings struct {
	gateway.RouterClass
	Letter string `json:"letter,omitempty"`
}

// RouterUpdate is the "router" part of PUT /admin/gateway; nil fields are
// unchanged. Classes, if set, replaces the whole list; ResetClasses
// restores the default classes (and is applied before Classes).
type RouterUpdate struct {
	Enabled      *bool                  `json:"enabled,omitempty"`
	Classifier   *string                `json:"classifier,omitempty"`
	Timeout      *string                `json:"timeout,omitempty"`
	Classes      *[]RouterClassSettings `json:"classes,omitempty"`
	ResetClasses bool                   `json:"reset_classes,omitempty"`
}

func routerSettings(c gateway.RouterConfig) RouterSettings {
	out := RouterSettings{Enabled: c.Enabled, Classifier: c.Classifier, Timeout: c.Timeout.String(),
		Classes: make([]RouterClassSettings, len(c.Classes))}
	for i, cl := range c.Classes {
		out.Classes[i] = RouterClassSettings{RouterClass: cl, Letter: gateway.RouterLetter(i)}
	}
	return out
}

var routerClassName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Limits that keep the cached classifier prompt short.
const (
	maxRouterDescription = 300
	maxRouterExamples    = 5
	maxRouterExample     = 300
)

// applyRouterUpdate returns c with u applied, or an error if the result is
// invalid.
func applyRouterUpdate(c gateway.RouterConfig, u RouterUpdate) (gateway.RouterConfig, error) {
	if u.Enabled != nil {
		c.Enabled = *u.Enabled
	}
	if u.Classifier != nil {
		c.Classifier = strings.TrimSpace(*u.Classifier)
	}
	if u.Timeout != nil {
		d, err := time.ParseDuration(*u.Timeout)
		if err != nil || d < time.Second || d > 10*time.Minute {
			return c, errors.New("router timeout must be a duration between 1s and 10m, e.g. \"20s\"")
		}
		c.Timeout = d
	}
	if u.ResetClasses {
		c.Classes = gateway.DefaultRouterClasses()
	}
	if u.Classes != nil {
		classes, err := routerClasses(*u.Classes)
		if err != nil {
			return c, err
		}
		c.Classes = classes
	}
	if c.Enabled && c.Classifier == "" {
		return c, errors.New(`the router needs a classifier target, e.g. "node/pixel"`)
	}
	if c.Enabled && len(c.Classes) < 2 {
		return c, errors.New("the router needs at least 2 classes")
	}
	return c, nil
}

// routerClasses validates and normalizes classes from the API.
func routerClasses(in []RouterClassSettings) ([]gateway.RouterClass, error) {
	if len(in) > gateway.MaxRouterClasses {
		return nil, fmt.Errorf("at most %d router classes", gateway.MaxRouterClasses)
	}
	seen := map[string]bool{}
	out := make([]gateway.RouterClass, 0, len(in))
	for _, s := range in {
		c := s.RouterClass
		c.Name = strings.TrimSpace(c.Name)
		c.Description = oneLine(c.Description)
		c.Target = strings.TrimSpace(c.Target)
		switch {
		case !routerClassName.MatchString(c.Name):
			return nil, fmt.Errorf("router class name %q: use 1-32 lower-case letters, digits and _, starting with a letter", c.Name)
		case c.Name == gateway.RouteFallback:
			return nil, fmt.Errorf("router class name %q is reserved", c.Name)
		case seen[c.Name]:
			return nil, fmt.Errorf("router class %q appears twice", c.Name)
		case c.Description == "" || len(c.Description) > maxRouterDescription:
			return nil, fmt.Errorf("router class %q needs a description of at most %d characters", c.Name, maxRouterDescription)
		case len(c.Examples) > maxRouterExamples:
			return nil, fmt.Errorf("router class %q: at most %d examples", c.Name, maxRouterExamples)
		}
		seen[c.Name] = true
		var ex []string
		for _, e := range c.Examples {
			if e = oneLine(e); e == "" {
				continue
			}
			if len(e) > maxRouterExample {
				return nil, fmt.Errorf("router class %q: examples must be at most %d characters", c.Name, maxRouterExample)
			}
			ex = append(ex, e)
		}
		c.Examples = ex
		out = append(out, c)
	}
	return out, nil
}

// oneLine trims s and joins its lines, so a class cannot break the
// classifier prompt's one-line-per-class layout.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// routerFromSettings turns stored settings back into a configuration; an
// invalid stored router is reported and left at the defaults.
func routerFromSettings(st RouterSettings) (gateway.RouterConfig, error) {
	enabled, classifier, timeout := st.Enabled, st.Classifier, st.Timeout
	classes := st.Classes
	return applyRouterUpdate(gateway.DefaultRouterConfig(), RouterUpdate{
		Enabled: &enabled, Classifier: &classifier, Timeout: &timeout, Classes: &classes})
}
