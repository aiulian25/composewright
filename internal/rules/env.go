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
			builder.reference(service, variable.Key, variable.Key, builder.knownDefault(variable.Key), false, at)
		case match != nil:
			name, operator, fallback := match[1], match[2], match[3]
			required := strings.Contains(operator, "?")
			if strings.Contains(operator, "?") {
				fallback = ""
			}
			builder.reference(service, variable.Key, name, firstNonEmpty(builder.knownDefault(name), fallback), required, at)
		default:
			for _, name := range extract.References(variable.Raw) {
				builder.ensureEntry(name, builder.knownDefault(name), false, at)
			}
			builder.reference(service, variable.Key, variable.Key, variable.Raw, true, at)
		}
	}
	for _, sample := range builder.loadedSamples(file, upstream) {
		builder.sampleEnvironment(file, service, sample)
	}
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
		builder.reference(service, entry.Key, entry.Key, entry.Value, false, at)
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

// loadedSamples finds the repository's sample for each env_file a service
// loads.
func (builder *builder) loadedSamples(file *extract.ComposeFile, service extract.ComposeService) []extract.EnvSample {
	var samples []extract.EnvSample
	directory := path.Dir(file.Path)
	for _, envFile := range service.EnvFiles {
		wanted := path.Clean(path.Join(directory, envFile))
		found := slices.IndexFunc(builder.facts.EnvSamples, func(sample extract.EnvSample) bool { return documents(sample.Path, wanted) })
		if found >= 0 {
			samples = append(samples, builder.facts.EnvSamples[found])
		}
	}
	return samples
}

// sampleWords mark a committed example of an env file in its name.
var sampleWords = []string{"example", "sample", "template", "dist"}

// documents is true when sample is the committed example of the env file a
// service loads, in the same folder: .env is documented by .env.example,
// .env.sample, example.env, sample.env, env.example and the like.
func documents(sample, wanted string) bool {
	if path.Dir(sample) != path.Dir(wanted) {
		return false
	}
	name, base := path.Base(sample), path.Base(wanted)
	bare := strings.TrimPrefix(base, ".")
	return name == base || strings.HasPrefix(name, base+".") || slices.ContainsFunc(sampleWords, func(word string) bool {
		return name == word+base || name == word+"."+bare || name == bare+"."+word
	})
}

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
	entry.Secret = ir.IsSecretSetting(name, value) || hasURLPassword(value)
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
			written := !entry.Commented && !strings.Contains(entry.Value, "$")
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
	if !entry.Secret || strings.Contains(literal, "$") || hasURLPassword(literal) {
		return
	}
	if _, mine := builder.own[entry.Key]; mine {
		entry.Value = ""
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

// knownDefault is the value upstream suggests for a variable: an env sample
// value, a compose default or a documented default.
func (builder *builder) knownDefault(name string) string {
	variable, found := builder.facts.Variable(name)
	if !found {
		return ""
	}
	return variable.Default
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
