package rules

import (
	"slices"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
)

// The suffixes of the settings an app reaches its database with, after a
// prefix such as DB or POSTGRES.
var (
	hostSuffixes     = []string{"HOST", "HOSTNAME", "SERVER", "ADDR", "ADDRESS"}
	portSuffixes     = []string{"PORT"}
	userSuffixes     = []string{"USER", "USERNAME", "LOGIN"}
	passwordSuffixes = []string{"PASSWORD", "PASS", "PASSWD", "PWD"}
	nameSuffixes     = []string{"DATABASE", "NAME", "DB", "DBNAME", "DATABASE_NAME"}
)

// addDatabase is R4 for an app that must be told where its database is, such
// as BookStack with its empty DB_HOST, in a stack that runs none: the
// database its settings name joins the stack, created with the app's own
// user, password and database name, and the app waits for it.
func (builder *builder) addDatabase() {
	app := builder.plan.AppService()
	hasDatabase := slices.ContainsFunc(builder.plan.Services, func(service ir.Service) bool { return service.Role == ir.RoleDB })
	if app == nil || hasDatabase {
		return
	}
	host, prefix, found := builder.emptyDatabaseHost(app)
	if !found {
		return
	}
	port := settingOf(app, prefix, portSuffixes)
	database, found := known.database(prefix, builder.valueOf(port))
	if !found {
		return
	}
	setup := database.Sidecar
	imageReference := database.Images[0] + ":" + setup.Tag
	at := ir.Provenance{Source: ir.SourceSidecarRule, Confidence: ir.ConfidenceMedium}
	image, _ := ir.ParseImage(imageReference)
	image.Provenance = at
	sidecar := ir.Service{Name: database.Name, Role: database.Role, Image: image, Provenance: at}
	appName := slug(lastSegment(app.Image.Repository))
	user, name := settingOf(app, prefix, userSuffixes), settingOf(app, prefix, nameSuffixes)
	builder.fill(host, database.Name)
	builder.fill(port, strconv.Itoa(setup.Port))
	builder.fill(user, appName)
	builder.fill(name, appName)
	links := []struct{ variable, setting string }{
		{setup.User, user}, {setup.Password, settingOf(app, prefix, passwordSuffixes)}, {setup.Database, name},
	}
	for _, link := range links {
		if link.setting != "" {
			builder.reference(&sidecar, link.variable, link.setting, "", false, at)
		}
	}
	if setup.RootPassword != "" {
		builder.reference(&sidecar, setup.RootPassword, setup.RootPassword, "", true, at)
	}
	app.DependsOn = append(app.DependsOn, ir.Dependency{Service: sidecar.Name, Condition: ir.ConditionStarted})
	builder.note(app, "%s was left for a database you run; the stack runs %s as %s and points %s at it", host, imageReference, sidecar.Name, host)
	builder.plan.Services = append(builder.plan.Services, sidecar)
}

// emptyDatabaseHost finds a required setting such as DB_HOST that upstream
// left empty, and the prefix the app's other database settings share.
func (builder *builder) emptyDatabaseHost(app *ir.Service) (string, string, bool) {
	for _, variable := range app.Env {
		prefix, isHost := databaseHostPrefix(variable.Key)
		entry, found := builder.plan.EnvEntry(variable.Ref)
		if isHost && found && entry.Required && entry.Value == "" {
			return variable.Ref, prefix, true
		}
	}
	return "", "", false
}

// databaseHostPrefix is DB for DB_HOST: the prefix of a setting that holds a
// database's address.
func databaseHostPrefix(key string) (string, bool) {
	for _, suffix := range hostSuffixes {
		prefix, isHost := strings.CutSuffix(key, "_"+suffix)
		if isHost && databaseName.MatchString(prefix) {
			return prefix, true
		}
	}
	return "", false
}

// settingOf is the .env entry behind the app's PREFIX_SUFFIX setting, or ""
// when the app has none.
func settingOf(app *ir.Service, prefix string, suffixes []string) string {
	for _, variable := range app.Env {
		if slices.ContainsFunc(suffixes, func(suffix string) bool { return variable.Key == prefix+"_"+suffix }) {
			return variable.Ref
		}
	}
	return ""
}

func (builder *builder) valueOf(name string) string {
	entry, found := builder.plan.EnvEntry(name)
	if !found {
		return ""
	}
	return entry.Value
}

// fill gives an empty .env entry the value the added database needs.
func (builder *builder) fill(name, value string) {
	entry, found := builder.plan.EnvEntry(name)
	if !found || entry.Value != "" {
		return
	}
	entry.Value = value
	entry.Provenance = ir.Provenance{Source: ir.SourceSidecarRule, SourceRef: entry.Provenance.SourceRef, Confidence: ir.ConfidenceMedium}
}
