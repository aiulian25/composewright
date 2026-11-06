package rules

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	minimumSecretLength = 4
	// requiredOperator and defaultOperator are Compose's ${NAME?message}
	// and ${NAME-default}, each also with a colon.
	requiredOperator = "?"
	defaultOperator  = "-"
	// ownEnvFile is where people keep their real values; samples such as
	// .env.example only suggest them.
	ownEnvFile = ".env"
	// laravelKey is the key a Laravel app such as BookStack encrypts with; it
	// must decode to exactly 32 bytes.
	laravelKey = "APP_KEY"
)

var (
	singleReference = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?])([^}]*))?\}?$`)
	publicURLName   = regexp.MustCompile(`^(BASE_URL|DOMAIN|APP_URL|PUBLIC_URL|SITE_URL|ROOT_URL|EXTERNAL_URL|SERVER_URL|WEB_URL|[A-Z0-9_]*_(BASE|PUBLIC|EXTERNAL|ROOT)_URL)$`)
	prefixedAddress = regexp.MustCompile(`^([A-Z0-9_]+)_(BASE|PUBLIC|EXTERNAL|ROOT)_URL$`)
	// genericAddressPrefixes say "this app" without naming it: APP_BASE_URL.
	genericAddressPrefixes = []string{"APP", "SERVER", "WEB", "SITE"}
	// listenSetting says where a program listens, such as LEAFWIKI_HOST or
	// BIND_ADDRESS; loopbackListen is a loopback value, with or without a port.
	listenSetting  = regexp.MustCompile(`^(?:([A-Z0-9_]+?)_)?(HOST|BIND|BIND_ADDRESS|BIND_ADDR|LISTEN|LISTEN_ADDRESS|LISTEN_ADDR|LISTEN_HOST)$`)
	loopbackListen = regexp.MustCompile(`^(127\.0\.0\.1|localhost|::1|\[::1\])(:[0-9]+)?$`)
	passwordWord   = regexp.MustCompile(`(^|_)(PASSWORD|PASSWD|PASS|PWD)(_|$)`)
	databaseName   = regexp.MustCompile(`(^|_)(POSTGRES|POSTGRESQL|PG|MYSQL|MARIADB|MONGO|MONGODB|REDIS|VALKEY|DB|DATABASE)(_|$)`)
	// appKey names keys an app signs or encrypts with; outsideCredential
	// names credentials that belong to a service outside the stack.
	appKey            = regexp.MustCompile(`(^|_)(SECRET|SECRET_KEY|SECRET_KEY_BASE|ENCRYPTION_KEY|SIGNING_KEY|SALT)$|^` + laravelKey + `$`)
	outsideCredential = regexp.MustCompile(`(^|_)(CLIENT|API|OAUTH|OIDC|SMTP|MAIL|EMAIL)(_|$)`)
	// ownLogin names the app's own first login, such as ADMIN_PASSWORD: the
	// stack owns it, so it is generated and DEPLOY.md says where it is.
	ownLogin = regexp.MustCompile(`(^|_)(ADMIN|ROOT|SUPERUSER|OWNER|INITIAL|DEFAULT)_(PASSWORD|PASSWD|PASS|PWD)$`)
	// connectionPassword finds scheme://user:${PASSWORD}@host in a value.
	connectionPassword = regexp.MustCompile(`://[^/\s:@]*:\$\{([A-Za-z_][A-Za-z0-9_]*)\}@([^/:?#\s]+)`)
	localHosts         = []string{"127.0.0.1", "localhost"}
	allInterfaces      = "0.0.0.0"
)

// composeEnvironment moves every variable of a compose service into the
// .env file and refers to it from compose.yaml, so the user edits one file.
// Variables of an env_file the service loads come from the repository's
// sample of that file.
func (builder *builder) composeEnvironment(file *extract.ComposeFile, service *ir.Service, upstream extract.ComposeService) {
	for _, variable := range upstream.Environment {
		at := provenance(ir.SourceCompose, variable.Location, ir.ConfidenceHigh)
		match := singleReference.FindStringSubmatch(variable.Raw)
		switch {
		case !variable.HasValue:
			value, source := builder.composeValue(file, variable.Key, "", "", at)
			builder.reference(service, variable.Key, variable.Key, value, false, source)
		case match != nil:
			name, operator, fallback := match[1], match[2], match[3]
			required := strings.Contains(operator, requiredOperator)
			value, at := builder.composeValue(file, name, operator, fallback, at)
			builder.reference(service, variable.Key, name, value, required, at)
		default:
			for _, name := range extract.References(variable.Raw) {
				value, source := builder.composeValue(file, name, "", "", at)
				builder.ensureEntry(name, value, false, source)
			}
			builder.localReference(service, variable.Key, variable.Raw, true, at)
		}
	}
	for _, sample := range builder.loadedSamples(file, upstream) {
		builder.sampleEnvironment(file, service, sample)
	}
}

// composeValue uses a real .env before Compose defaults. In the absence of
// that file, matching examples are inference only; explicit nonempty Compose
// fallbacks remain stronger evidence than example values.
func (builder *builder) composeValue(file *extract.ComposeFile, name, operator, fallback string, at ir.Provenance) (string, ir.Provenance) {
	if operator == "" && fallback == "" {
		for _, mention := range file.Variables {
			if mention.Name == name && mention.Default != "" {
				fallback = mention.Default
				break
			}
		}
	}
	wanted := path.Join(path.Dir(file.Path), ownEnvFile)
	sample, found := extract.MatchingEnvSample(builder.facts.EnvSamples, wanted)
	if found {
		for i := len(sample.Entries) - 1; i >= 0; i-- {
			entry := sample.Entries[i]
			if entry.Commented || entry.Key != name {
				continue
			}
			actual := sample.Path == wanted
			if !actual && strings.Contains(operator, defaultOperator) && fallback != "" {
				return fallback, at
			}
			if actual && entry.Value == "" && strings.HasPrefix(operator, ":") && strings.Contains(operator, defaultOperator) {
				return fallback, at
			}
			confidence := ir.ConfidenceMedium
			if actual {
				confidence = ir.ConfidenceHigh
			}
			return entry.Value, provenance(ir.SourceEnvSample, entry.Location, confidence)
		}
	}
	return fallback, at
}

// sampleEnvironment gives a service the variables of an env_file it loads,
// as Docker does, except those that only configure the compose file.
func (builder *builder) sampleEnvironment(file *extract.ComposeFile, service *ir.Service, sample extract.EnvSample) {
	composeOnly := composeOnlyVariables(file)
	for _, entry := range sample.Entries {
		used := slices.ContainsFunc(service.Env, func(existing ir.EnvRef) bool { return existing.Key == entry.Key })
		if entry.Commented || composeOnly[entry.Key] || used {
			continue
		}
		at := provenance(ir.SourceEnvSample, entry.Location, ir.ConfidenceMedium)
		builder.localReference(service, entry.Key, entry.Value, false, at)
	}
}

// composeOnlyVariables are those a compose file interpolates only outside
// environment values: a host port, a container name, a folder or an image
// tag. They configure the compose file, which composewright writes itself. A
// variable an environment value uses is a setting of the app, so an
// env_file passes it too: immich's server reads DB_PASSWORD from .env while
// the database gets it as POSTGRES_PASSWORD: ${DB_PASSWORD}.
func composeOnlyVariables(file *extract.ComposeFile) map[string]bool {
	inEnvironment := map[string]bool{}
	for _, service := range file.Services {
		for _, variable := range service.Environment {
			for _, name := range extract.References(variable.Raw) {
				inEnvironment[name] = true
			}
		}
	}
	only := map[string]bool{}
	for _, mention := range file.Variables {
		only[mention.Name] = !inEnvironment[mention.Name]
	}
	return only
}

// loadedSamples selects exact files before examples and keeps only winning
// assignments: later files and later assignments override earlier values.
func (builder *builder) loadedSamples(file *extract.ComposeFile, service extract.ComposeService) []extract.EnvSample {
	var samples []extract.EnvSample
	for _, envFile := range service.EnvFiles {
		sample, found := extract.MatchingEnvSample(builder.facts.EnvSamples, path.Clean(path.Join(path.Dir(file.Path), envFile)))
		if found {
			samples = append(samples, sample)
		}
	}
	seen := map[string]bool{}
	for i := len(samples) - 1; i >= 0; i-- {
		original := samples[i].Entries
		var kept []extract.EnvSampleEntry
		for j := len(original) - 1; j >= 0; j-- {
			entry := original[j]
			if entry.Commented || seen[entry.Key] {
				continue
			}
			seen[entry.Key] = true
			kept = append(kept, entry)
		}
		slices.Reverse(kept)
		samples[i].Entries = kept
	}
	return samples
}

func documents(sample, wanted string) bool { return extract.DocumentsEnv(sample, wanted) }

// reference adds name to the .env file (once) and makes the service read key
// from it.
func (builder *builder) reference(service *ir.Service, key, name, value string, required bool, at ir.Provenance) {
	entry := builder.ensureEntry(name, value, required, at)
	if slices.ContainsFunc(service.Env, func(existing ir.EnvRef) bool { return existing.Key == key }) {
		return
	}
	service.Env = append(service.Env, ir.EnvRef{Key: key, Ref: name, Required: entry.Required, Secret: entry.Secret,
		Description: entry.Description, Provenance: at})
}

// ensureEntry creates the .env entry for name. A literal secret written
// upstream (a sample password) is never kept: the entry generates a fresh
// one, and a second variable with the same literal refers to the first.
func (builder *builder) ensureEntry(name, value string, required bool, at ir.Provenance) *ir.EnvFileEntry {
	if entry, found := builder.plan.EnvEntry(name); found {
		entry.Required = entry.Required || required
		return entry
	}
	variable, _ := builder.facts.Variable(name)
	entry := ir.EnvFileEntry{Key: name, Value: value, Required: required, Description: variable.Description, Provenance: at}
	if entry.Description != "" && at.Confidence == ir.ConfidenceHigh {
		entry.Provenance.SourceRef = at.SourceRef
	}
	entry.Secret = HoldsSecret(name, value)
	builder.secretValue(&entry)
	builder.plan.EnvFile = append(builder.plan.EnvFile, entry)
	created, _ := builder.plan.EnvEntry(name)
	return created
}

// OwnSecrets are the secrets a person wrote in their own .env, in a folder
// on this machine or an upload. A fetched repository's files are public, so
// its values are only samples. Placeholders such as changeme are left out:
// the plan generates those.
func OwnSecrets(facts *extract.Facts, source *fetch.Source) map[string]string {
	own := map[string]string{}
	if facts == nil || source == nil || source.Kind != fetch.SourceLocal {
		return own
	}
	for _, sample := range facts.EnvSamples {
		if path.Base(sample.Path) != ownEnvFile {
			continue
		}
		for _, entry := range sample.Entries {
			written := !entry.Commented
			if written && ir.IsSecretSetting(entry.Key, entry.Value) && !ir.IsPlaceholderSecret(entry.Value) {
				own[entry.Key] = entry.Value
			}
		}
	}
	return own
}

// secretValue replaces a secret written upstream; generateStackSecrets
// decides about empty ones once the whole stack is known. A secret from the
// person's own .env stays theirs: the plan records it as supplied, and the
// value goes to the encrypted store, never into the plan.
func (builder *builder) secretValue(entry *ir.EnvFileEntry) {
	literal := entry.Value
	if entry.Secret {
		if _, mine := builder.own[entry.Key]; mine {
			entry.Value = ""
			return
		}
	}
	if !entry.Secret || strings.Contains(literal, "$") || hasURLPassword(literal) {
		return
	}
	owner, found := builder.secretOwners[literal]
	// Upstream lets the user set each of two variables it reads by name, as
	// ${REDIS_PASSWORD:-snapotter} and ${POSTGRES_PASSWORD:-snapotter}: an
	// equal fallback is a shared placeholder, so each gets its own secret.
	bothNamed := builder.interpolated(entry.Key) && builder.interpolated(owner)
	if found && len(literal) >= minimumSecretLength && !bothNamed {
		entry.Value = "${" + owner + "}"
		return
	}
	if literal == "" && !entry.Required {
		return
	}
	entry.Value, entry.Generate = "", generatorFor(entry.Key)
	if !found && len(literal) >= minimumSecretLength {
		builder.secretOwners[literal] = entry.Key
	}
}

// interpolated is true when a compose file of the repository reads name as
// ${name}.
func (builder *builder) interpolated(name string) bool {
	return slices.ContainsFunc(builder.facts.ComposeFiles, func(file extract.ComposeFile) bool {
		return slices.ContainsFunc(file.Variables, func(mention extract.Mention) bool { return mention.Name == name })
	})
}

// generateStackSecrets fills the empty secrets the stack owns: keys the app
// signs or encrypts with, the app's own admin password, and passwords of a
// database or cache that runs in this stack, as a service of its own or
// inside a container that connects to it on localhost. Credentials of
// outside services stay with the user, who fills them in on the review.
func (builder *builder) generateStackSecrets() {
	ownDatabase := slices.ContainsFunc(builder.plan.Services, func(service ir.Service) bool {
		return service.Role == ir.RoleDB || service.Role == ir.RoleCache
	})
	inStack := builder.stackConnectionPasswords()
	for index := range builder.plan.EnvFile {
		entry := &builder.plan.EnvFile[index]
		_, mine := builder.own[entry.Key]
		empty := entry.Secret && entry.Value == "" && entry.Generate == "" && !mine
		databasePassword := databaseName.MatchString(entry.Key) && passwordWord.MatchString(entry.Key) && ownDatabase
		owned := appKey.MatchString(entry.Key) || ownLogin.MatchString(entry.Key) || databasePassword || inStack[entry.Key]
		if empty && owned && !outsideCredential.MatchString(entry.Key) {
			entry.Generate = generatorFor(entry.Key)
		}
	}
}

// stackConnectionPasswords names the variables that are the password of a
// connection string to localhost or to a service of this stack.
func (builder *builder) stackConnectionPasswords() map[string]bool {
	hosts := map[string]bool{}
	for _, host := range localHosts {
		hosts[host] = true
	}
	for _, service := range builder.plan.Services {
		hosts[service.Name] = true
	}
	passwords := map[string]bool{}
	for _, entry := range builder.plan.EnvFile {
		for _, match := range connectionPassword.FindAllStringSubmatch(entry.Value, -1) {
			if hosts[strings.ToLower(match[2])] {
				passwords[match[1]] = true
			}
		}
	}
	return passwords
}

// generatorFor picks the secret format: people type admin passwords, so they
// are alphanumeric; a Laravel key has its own form; everything else gets 32
// random bytes as hex.
func generatorFor(name string) string {
	if name == laravelKey {
		return ir.GenerateLaravelKey
	}
	if passwordWord.MatchString(name) && !databaseName.MatchString(name) {
		return ir.GeneratePassword24
	}
	return ir.GenerateHex32
}

// linkSecrets rewrites connection strings that embedded an upstream sample
// password, e.g. postgres://u:secret@db/x becomes
// postgres://u:${POSTGRES_PASSWORD}@db/x once POSTGRES_PASSWORD is generated.
func (builder *builder) linkSecrets() {
	for index := range builder.plan.EnvFile {
		entry := &builder.plan.EnvFile[index]
		parsed, err := url.Parse(entry.Value)
		if err != nil || parsed.User == nil {
			continue
		}
		password, hasPassword := parsed.User.Password()
		if !hasPassword {
			continue
		}
		owner, known := builder.secretOwners[password]
		if !known {
			owner = builder.passwordEntry(entry.Key, entry.Provenance)
			// The new entry may have moved the .env entries.
			entry = &builder.plan.EnvFile[index]
		}
		user := parsed.User.Username()
		entry.Value = strings.Replace(entry.Value, "://"+user+":"+password+"@", "://"+user+":${"+owner+"}@", 1)
		entry.Secret, entry.Generate = true, ""
	}
}

// passwordEntry holds the password of a connection string to a service that
// is not part of the stack; the user must supply it, nothing is generated.
func (builder *builder) passwordEntry(key string, at ir.Provenance) string {
	name := key + "_PASSWORD"
	if _, found := builder.plan.EnvEntry(name); !found {
		builder.plan.EnvFile = append(builder.plan.EnvFile, ir.EnvFileEntry{Key: name, Secret: true, Required: true,
			Description: "Password inside " + key + "; the database it points to is not part of this stack", Provenance: at})
	}
	return name
}

// HoldsSecret reports whether a .env entry's value must be sealed: secret
// material under its name, or a URL that carries a password.
func HoldsSecret(name, value string) bool {
	return ir.IsSecretSetting(name, value) || hasURLPassword(value)
}

func hasURLPassword(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return false
	}
	_, hasPassword := parsed.User.Password()
	return hasPassword
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// secretFiles passes a secret as a file when the upstream documents its
// *_FILE twin, as POSTGRES_PASSWORD_FILE: compose.yaml mounts it from
// secrets/ and names the file in the twin, and .env no longer holds it.
// The twin a person may set by hand leaves the plan.
func (builder *builder) secretFiles() {
	twins := map[string]string{}
	for _, entry := range builder.plan.EnvFile {
		variable, _ := builder.facts.Variable(entry.Key)
		if variable.FileTwin != "" && passableAsFile(builder.plan, entry) {
			twins[entry.Key] = variable.FileTwin
		}
	}
	for key, twin := range twins {
		dropVariable(builder.plan, twin)
		entry, _ := builder.plan.EnvEntry(key)
		entry.FileEnvKey = twin
	}
}

// passableAsFile: the stack generates the secret, so its file is never
// empty: an app reads an empty *_FILE as an error, as OpenTodo does, where an
// empty variable is a setting left off, so a credential a person supplies
// stays in .env. Every service reads it under its own name, the one the twin
// belongs to, and nothing else may interpolate it, as a connection string, a
// command or a healthcheck reading $${PASSWORD} in the container, since
// neither .env nor the environment would hold it any more.
func passableAsFile(plan *ir.IR, entry ir.EnvFileEntry) bool {
	if !entry.Secret || entry.Value != "" || entry.Generate == "" {
		return false
	}
	read := false
	for _, service := range plan.Services {
		for _, variable := range service.Env {
			if variable.Ref == entry.Key && variable.Key != entry.Key {
				return false
			}
			read = read || variable.Ref == entry.Key
		}
	}
	return read && !interpolated(plan, entry.Key)
}

// interpolated reports whether a value, a literal, a command, an entrypoint
// or a healthcheck of the plan reads name, as ${name} or $name.
func interpolated(plan *ir.IR, name string) bool {
	use := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `\b`)
	texts := []string{}
	for _, entry := range plan.EnvFile {
		texts = append(texts, entry.Value)
	}
	for _, service := range plan.Services {
		for _, variable := range service.Env {
			texts = append(texts, variable.Literal)
		}
		texts = slices.Concat(texts, service.Command, service.Entrypoint, service.Healthcheck.Test)
	}
	return slices.ContainsFunc(texts, use.MatchString)
}

// dropVariable takes a variable out of the plan: its .env entry, the
// services reading it and its optional setting.
func dropVariable(plan *ir.IR, name string) {
	plan.EnvFile = slices.DeleteFunc(plan.EnvFile, func(entry ir.EnvFileEntry) bool { return entry.Key == name })
	for index := range plan.Services {
		service := &plan.Services[index]
		service.Env = slices.DeleteFunc(service.Env, func(variable ir.EnvRef) bool { return variable.Key == name || variable.Ref == name })
	}
	plan.OptionalEnv = slices.DeleteFunc(plan.OptionalEnv, func(setting ir.OptionalSetting) bool { return setting.Key == name })
}
