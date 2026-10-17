package rules

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	optionalNote         = "optional: upstream profile %s"
	offDependencyNote    = "upstream waits for %s, an optional service that is off; switch it on to keep that"
	unknownNameSeparator = ", "
)

// ErrNotOptional names a service the plan does not offer as optional.
var ErrNotOptional = errors.New("the plan has no such optional service")

// optionalServices describes the services of the file upstream starts only
// with a profile, each with the optional services it depends on.
func optionalServices(file *extract.ComposeFile) []ir.OptionalService {
	described := []ir.OptionalService{}
	for _, service := range file.Services {
		if !service.Optional() {
			continue
		}
		dependencies := []string{}
		for _, dependency := range service.DependsOn {
			if optionalIn(file, dependency.Service) {
				dependencies = append(dependencies, strings.ToLower(dependency.Service))
			}
		}
		described = append(described, ir.OptionalService{Name: strings.ToLower(service.Name), Image: service.Image,
			Profiles: service.Profiles, DependsOn: dependencies})
	}
	return described
}

func optionalIn(file *extract.ComposeFile, name string) bool {
	return slices.ContainsFunc(file.Services, func(service extract.ComposeService) bool {
		return service.Optional() && strings.EqualFold(service.Name, name)
	})
}

// Enabled are the optional services to switch on for the names given, in
// any case: those, and the optional services they depend on directly or
// through another, spelt as the plan spells them and in its order. A name
// the plan does not offer is an error; the others are still returned.
func Enabled(offered []ir.OptionalService, names []string) ([]string, error) {
	wanted := map[string]bool{}
	var unknown []string
	for _, name := range names {
		index := slices.IndexFunc(offered, func(service ir.OptionalService) bool { return strings.EqualFold(service.Name, name) })
		if index < 0 {
			unknown = append(unknown, name)
			continue
		}
		wanted[offered[index].Name] = true
	}
	for added := true; added; {
		added = false
		for _, service := range offered {
			missing := slices.DeleteFunc(slices.Clone(service.DependsOn), func(dependency string) bool { return wanted[dependency] })
			if !wanted[service.Name] || len(missing) == 0 {
				continue
			}
			for _, dependency := range missing {
				wanted[dependency] = true
			}
			added = true
		}
	}
	enabled := []string{}
	for _, service := range offered {
		if wanted[service.Name] {
			enabled = append(enabled, service.Name)
		}
	}
	if len(unknown) > 0 {
		return enabled, fmt.Errorf("%w: %s", ErrNotOptional, strings.Join(unknown, unknownNameSeparator))
	}
	return enabled, nil
}

// Running are the optional services plan runs, in the order it offers them.
func Running(plan *ir.IR) []string {
	running := []string{}
	for _, optional := range plan.OptionalServices {
		if _, found := plan.Service(optional.Name); found {
			running = append(running, optional.Name)
		}
	}
	return running
}
