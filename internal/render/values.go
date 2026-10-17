package render

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/secrets"
)

const (
	variableTimezone = "TZ"
	variableUserID   = "PUID"
	variableGroupID  = "PGID"
	httpsScheme      = "https://"
)

// wholeReference is a value that is only a reference to another variable,
// as a Review edit such as DB_PASSWORD=${POSTGRES_PASSWORD} stores it.
var wholeReference = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// Values decides every .env value. A secret stored for this project before
// is reused, so regenerating never rotates a password already deployed;
// other secrets are generated; a secret the plan cannot generate keeps the
// value a person typed in; TZ, PUID and PGID take the profile defaults when
// the image reads them; URL placeholders follow the chosen exposure.
func Values(plan *ir.IR, options Options, stored map[string]string) (map[string]string, error) {
	values := make(map[string]string, len(plan.EnvFile))
	for _, entry := range plan.EnvFile {
		value, err := entryValue(plan, entry, options, stored)
		if err != nil {
			return nil, err
		}
		values[entry.Key] = value
	}
	return values, nil
}

// danglingReference reports whether a stored value only refers to a
// variable the .env no longer has, or to its own key: what the entry was
// before, as DB_PASSWORD=${POSTGRES_PASSWORD} with the database switched
// off, not a secret to keep. A reference to a variable still in the .env is
// kept, and a secret that only holds a $, as Sup3r$ecret, is one.
func danglingReference(plan *ir.IR, key, value string) bool {
	match := wholeReference.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	_, defined := plan.EnvEntry(match[1])
	return !defined || match[1] == key
}

func entryValue(plan *ir.IR, entry ir.EnvFileEntry, options Options, stored map[string]string) (string, error) {
	defaults := options.Profile.Defaults
	previous, hasPrevious := stored[entry.Key]
	hasPrevious = hasPrevious && !danglingReference(plan, entry.Key, previous)
	switch {
	case entry.Generate != "" && hasPrevious && previous != "":
		return previous, nil
	case entry.Generate != "":
		return secrets.Generate(entry.Generate)
	case entry.Secret && entry.Value == "" && hasPrevious:
		return previous, nil
	case entry.Provenance.Source == ir.SourceUser:
		return entry.Value, nil
	case entry.Key == variableTimezone && defaults.TZ != "":
		return defaults.TZ, nil
	case entry.Key == variableUserID:
		return strconv.Itoa(defaults.PUID), nil
	case entry.Key == variableGroupID:
		return strconv.Itoa(defaults.PGID), nil
	case rules.IsPublicURL(entry.Key) && entry.Provenance.Source == ir.SourceDefault:
		return cmp.Or(options.PublicURL(plan.App.Name), entry.Value), nil
	}
	return entry.Value, nil
}

// PublicURL is the address people open, as https://home.example.com/rss;
// "" when the app keeps its loopback port.
func (options Options) PublicURL(project string) string {
	address := options.PublicAddress(project)
	if address == "" {
		return ""
	}
	return httpsScheme + address
}

// PublicAddress is the host and path people open the app at: the one a
// person set, else through the first proxy or tunnel the app joins that
// has a domain, else the first it joins; "" when it joins no network and
// no host was set.
func (options Options) PublicAddress(project string) string {
	if options.PublicHost != "" {
		return options.PublicHost
	}
	networks := options.networks()
	if len(networks) == 0 {
		return ""
	}
	withDomain := slices.IndexFunc(networks, func(network config.ProfileNetwork) bool { return network.Domain != "" })
	return options.networkHost(networks[max(withDomain, 0)], project)
}
