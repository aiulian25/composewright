package rules

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
)

// allocateLocalNames reserves source identifiers before choosing output names.
// Conflicting literals belong to their services; explicit interpolation and
// a common env_file retain their shared source. Include optional services so toggling one cannot rename
// another service's inputs.
func (builder *builder) allocateLocalNames(file *extract.ComposeFile) {
	locals := map[string]map[string]string{}
	sources := map[string]map[string]string{}
	reserved := map[string]bool{}
	shared := map[string]bool{}
	composeOnly := composeOnlyVariables(file)
	for _, variable := range builder.facts.Variables {
		reserved[variable.Name] = true
	}
	for _, service := range file.Services {
		seen := map[string]bool{}
		add := func(key, value, source string) {
			if seen[key] {
				return
			}
			seen[key] = true
			if locals[key] == nil {
				locals[key] = map[string]string{}
				sources[key] = map[string]string{}
			}
			locals[key][service.Name] = value
			sources[key][service.Name] = source
			reserved[key] = true
		}
		for _, variable := range service.Environment {
			reserved[variable.Key] = true
			if !variable.HasValue {
				shared[variable.Key] = true
			}
			for _, name := range extract.References(variable.Raw) {
				reserved[name], shared[name] = true, true
			}
			if variable.HasValue && singleReference.FindStringSubmatch(variable.Raw) == nil {
				add(variable.Key, variable.Raw, "literal:"+service.Name)
			} else {
				seen[variable.Key] = true
			}
		}
		for _, sample := range builder.loadedSamples(file, service) {
			for _, entry := range sample.Entries {
				if !entry.Commented && !composeOnly[entry.Key] {
					// The Compose interpolation .env and this env_file name
					// the same source. Other env_files remain service-local.
					source := entry.Location.String()
					if documents(sample.Path, path.Join(path.Dir(file.Path), ownEnvFile)) {
						source = "global:" + entry.Key
					}
					add(entry.Key, entry.Value, source)
				}
			}
		}
	}
	builder.localNames = map[string]map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(locals)) {
		consumers := locals[key]
		if len(consumers) < 2 && !shared[key] {
			continue
		}
		sameSource := ""
		common := true
		for _, source := range sources[key] {
			if sameSource == "" {
				sameSource = source
			}
			common = common && source == sameSource
		}
		if common && (!shared[key] || sameSource == "global:"+key) {
			continue
		}
		// Preserve existing equal-value groups, including logical credentials
		// already linked by secretValue. Split only a demonstrated conflict.
		first := ""
		equal := true
		initialized := false
		for _, value := range consumers {
			if !initialized {
				first, initialized = value, true
			}
			equal = equal && value == first
		}
		if equal && !shared[key] {
			continue
		}
		for _, service := range slices.Sorted(maps.Keys(consumers)) {
			prefix := strings.Map(func(r rune) rune {
				if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
					return unicode.ToUpper(r)
				}
				return '_'
			}, service)
			if prefix == "" || (prefix[0] >= '0' && prefix[0] <= '9') {
				prefix = "SERVICE_" + prefix
			}
			candidate := prefix + "_" + key
			if len(candidate) > 128 {
				suffix := sha256.Sum256([]byte(service + "\x00" + key))
				candidate = fmt.Sprintf("%s_%x", candidate[:119], suffix[:4])
			}
			if reserved[candidate] {
				// Hash only the public service identity, never an environment value.
				suffix := sha256.Sum256([]byte(service + "\x00" + key))
				if len(candidate) > 119 {
					candidate = candidate[:119]
				}
				candidate = fmt.Sprintf("%s_%x", candidate, suffix[:4])
				base := candidate
				if len(base) > 100 {
					base = base[:100]
				}
				for counter := 1; reserved[candidate]; counter++ {
					candidate = fmt.Sprintf("%s_%d", base, counter)
				}
			}
			reserved[candidate] = true
			normalized := strings.ToLower(service)
			if builder.localNames[normalized] == nil {
				builder.localNames[normalized] = map[string]string{}
			}
			builder.localNames[normalized][key] = candidate
		}
	}
}

func (builder *builder) localReference(service *ir.Service, key, value string, required bool, at ir.Provenance) {
	name := builder.localNames[service.Name][key]
	if name == "" {
		name = key
	}
	if name != key {
		if _, supplied := builder.own[key]; supplied {
			builder.ensureEntry(key, value, required, at)
			value = "${" + key + "}"
		}
	}
	builder.reference(service, key, name, value, required, at)
	if name != key {
		entry, _ := builder.plan.EnvEntry(name)
		if entry.Generate != "" {
			entry.Generate = generatorFor(key)
		}
		variable, _ := builder.facts.Variable(key)
		entry.Description = strings.TrimSpace(fmt.Sprintf("%s for service %s. %s", key, service.Name, variable.Description))
	}
}
