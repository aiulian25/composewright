package render

import (
	"fmt"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/rules"
)

// noWebPortLine is the Open it line for an app that has neither a published
// port nor a web port another container could reach.
const noWebPortLine = "No port is published, and the app has no web port to open."

// Deploy writes DEPLOY.md: how to start the stack, where it is reached, what
// to do on the proxy side, storage preparation and first-login steps.
func Deploy(plan *ir.IR, options Options, status string) []byte {
	var text strings.Builder
	write := func(format string, arguments ...any) { fmt.Fprintf(&text, format+"\n", arguments...) }
	write("# %s", plan.App.Name)
	write("")
	if plan.App.Summary != "" {
		write("%s", singleLine(plan.App.Summary))
		write("")
	}
	if plan.App.UpstreamURL != "" {
		write("Upstream: %s", plan.App.UpstreamURL)
	}
	write("Validation: %s", status)
	write("")
	write("## Start it")
	write("")
	write("In the folder that holds compose.yaml and .env:")
	write("")
	write("    docker compose up -d")
	write("    docker compose ps")
	write("")
	options.reachSection(plan, write)
	options.storageSection(plan, write)
	needsSection(plan, options, write)
	optionalSection(plan, write)
	notesSection(plan, write)
	write("## Secrets")
	write("")
	write(".env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;")
	write("composewright keeps an encrypted copy, so `composewright show %s --reveal` prints them again.", plan.App.Name)
	return []byte(text.String())
}

func (options Options) reachSection(plan *ir.IR, write func(string, ...any)) {
	app := plan.AppService()
	if app == nil {
		return
	}
	write("## Open it")
	write("")
	port, hasWebPort := webPort(app)
	networks := options.networks()
	for _, network := range networks {
		line := fmt.Sprintf("- The app joins the external network `%s`.", network.Name)
		if hasWebPort {
			line += " " + proxyStep(network, options.networkHost(network, plan.App.Name), options.publicPath(), app.Name, port)
		}
		write("%s", line)
	}
	if len(networks) > 0 {
		write("")
		write("These networks must exist before the stack starts; create one your proxy has not made yet with `docker network create <name>`.")
		write("")
	}
	hostPort := publishedHostPort(app)
	internalPort, hasInternalPort := containerWebPort(app)
	unpublished := options.ExposeVia == ExposeNone || hostPort == 0
	switch {
	case unpublished && len(networks) > 0:
		write("No port is published; the networks above are the way in.")
	case unpublished && !hasInternalPort:
		write(noWebPortLine)
	case unpublished:
		write("No port is published. Reach it from another container on the same network at http://%s:%d.", app.Name, internalPort)
	case options.ExposeVia == ExposeLAN:
		write("Published on every interface: http://<this-host>:%d. Put a reverse proxy with TLS in front before exposing it further.", hostPort)
	default:
		write("Published on this machine only: http://localhost:%d", hostPort)
	}
	servedOnPort := options.PublicHost != "" && len(networks) == 0 && options.ExposeVia != ExposeNone
	if servedOnPort {
		write("Point https://%s at the published port.", options.PublicHost)
	}
	write("")
}

func proxyStep(network config.ProfileNetwork, host, path, service string, port int) string {
	target := fmt.Sprintf("http://%s:%d", service, port)
	if step, found := pathStep(network, host, path, target); found {
		return step
	}
	switch network.Provider {
	case providerTraefik:
		return fmt.Sprintf("Traefik routes https://%s to it through the labels in compose.yaml.", host+path)
	case providerNPM:
		return fmt.Sprintf("In Nginx Proxy Manager, add a proxy host %s that forwards to %s.", host, target)
	case providerCaddy:
		return fmt.Sprintf("In your Caddyfile add:\n\n    %s {\n        reverse_proxy %s:%d\n    }", host, service, port)
	case providerCloudflare:
		return fmt.Sprintf("In the cloudflared ingress rules add: hostname %s, service %s.", host, target)
	case providerPangolin:
		return fmt.Sprintf("In Pangolin, add a resource for %s with the target %s (newt joins the same network).", host+path, target)
	case providerTailscale:
		return fmt.Sprintf("Reach it through your tailnet at %s.", target)
	}
	return fmt.Sprintf("Point %s at %s.", host+path, target)
}

// pathStep is the proxy step for an app served under a path, for the
// providers whose set-up differs: the path is stripped before the request
// reaches the app, as the Traefik labels do. Without a path there is none.
func pathStep(network config.ProfileNetwork, host, path, target string) (string, bool) {
	if path == "" {
		return "", false
	}
	switch network.Provider {
	case providerNPM:
		return fmt.Sprintf("In Nginx Proxy Manager, add a proxy host %s with a custom location %s/ that forwards to %s/.", host, path, target), true
	case providerCaddy:
		return fmt.Sprintf("In your Caddyfile add:\n\n    %s {\n        redir %s %s/\n        handle_path %s/* {\n            reverse_proxy %s\n        }\n    }",
			host, path, path, path, target), true
	case providerCloudflare:
		return fmt.Sprintf("In the cloudflared ingress rules add: hostname %s, path ^%s, service %s. cloudflared keeps the path, so the app must serve under %s.", host, path, target, path), true
	}
	return "", false
}

func (options Options) storageSection(plan *ir.IR, write func(string, ...any)) {
	if options.Storage != config.StorageBind {
		write("## Data")
		write("")
		write("Data lives in named Docker volumes (`docker volume ls | grep %s`); `docker compose down -v` deletes them.", plan.App.Name)
		write("")
		return
	}
	storage := options.Profile.Storage
	write("## Prepare the data folders")
	write("")
	write("Create the bind-mounted folders before the first start:")
	write("")
	for _, service := range plan.Services {
		owner := folderOwner(service, storage.Owner)
		for _, volume := range service.Volumes {
			path := BindSource(storage, plan.App.Name, volume.Name)
			write("    mkdir -p %s", path)
			if owner != "" {
				write("    sudo chown %s %s", owner, path)
			}
		}
	}
	write("")
}

// folderOwner is who must own a service's bind-mounted folders: the user it
// runs as when that is fixed, else the profile's owner.
func folderOwner(service ir.Service, profileOwner string) string {
	if service.User != "" {
		return service.User
	}
	return profileOwner
}

func needsSection(plan *ir.IR, options Options, write func(string, ...any)) {
	if len(plan.Needs) == 0 {
		return
	}
	write("## Privileges")
	write("")
	for _, need := range plan.Needs {
		state := "requested, not granted"
		if options.isGranted(need.Capability) {
			state = "granted by you"
		}
		capability := need.Capability
		if need.Detail != "" {
			capability += " `" + need.Detail + "`"
		}
		write("- %s for %s (%s): %s", capability, need.Service, state, singleLine(need.Reason))
	}
	write("")
}

func notesSection(plan *ir.IR, write func(string, ...any)) {
	var notes []string
	notes = append(notes, plan.PostInstallNotes...)
	for _, service := range plan.Services {
		for _, note := range service.Notes {
			notes = append(notes, service.Name+": "+note)
		}
	}
	if len(notes) == 0 {
		return
	}
	write("## After the first start")
	write("")
	for _, note := range notes {
		write("- %s", singleLine(note))
	}
	write("")
}

// containerWebPort is the first TCP port the app listens on, published or
// not: where another container on its network reaches it.
func containerWebPort(service *ir.Service) (int, bool) {
	for _, port := range service.Ports {
		if port.Protocol == ir.ProtocolTCP {
			return port.Container, true
		}
	}
	return 0, false
}

func publishedHostPort(service *ir.Service) int {
	for _, port := range service.Ports {
		if port.Publish {
			return rules.HostPort(port)
		}
	}
	return 0
}
