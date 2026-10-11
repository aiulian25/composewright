package render

import (
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

func entryValue(plan *ir.IR, entry ir.EnvFileEntry, options Options, stored map[string]string) (string, error) {
	defaults := options.Profile.Defaults
	previous, hasPrevious := stored[entry.Key]
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
		return options.publicURL(plan, entry.Value), nil
	}
	return entry.Value, nil
}

// publicURL is the address people open: through the first proxy or tunnel
// the app joins that has a domain, else the first it joins, else the
// loopback port.
func (options Options) publicURL(plan *ir.IR, current string) string {
	networks := options.networks()
	if len(networks) == 0 {
		return current
	}
	withDomain := slices.IndexFunc(networks, func(network config.ProfileNetwork) bool { return network.Domain != "" })
	return httpsScheme + options.networkHost(networks[max(withDomain, 0)], plan.App.Name)
}
