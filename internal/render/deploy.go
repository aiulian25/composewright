package render

import (
	"fmt"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/rules"
)

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
			line += " " + proxyStep(network, options.networkHost(network, plan.App.Name), app.Name, port)
		}
		write("%s", line)
	}
	if len(networks) > 0 {
		write("")
		write("These networks must exist before the stack starts; create one your proxy has not made yet with `docker network create <name>`.")
		write("")
	}
	switch {
	case options.ExposeVia == ExposeNone && len(networks) > 0:
		write("No port is published; the networks above are the way in.")
	case options.ExposeVia == ExposeNone:
		write("No port is published. Reach it from another container on the same network at http://%s:%d.", app.Name, port)
	case options.ExposeVia == ExposeLAN:
		write("Published on every interface: http://<this-host>:%d. Put a reverse proxy with TLS in front before exposing it further.", publishedHostPort(app))
	default:
		write("Published on this machine only: http://localhost:%d", publishedHostPort(app))
	}
	write("")
}

func proxyStep(network config.ProfileNetwork, host, service string, port int) string {
	target := fmt.Sprintf("http://%s:%d", service, port)
	switch network.Provider {
	case providerTraefik:
		return fmt.Sprintf("Traefik routes https://%s to it through the labels in compose.yaml.", host)
	case providerNPM:
		return fmt.Sprintf("In Nginx Proxy Manager, add a proxy host %s that forwards to %s.", host, target)
	case "caddy":
		return fmt.Sprintf("In your Caddyfile add:\n\n    %s {\n        reverse_proxy %s:%d\n    }", host, service, port)
	case "cloudflare":
		return fmt.Sprintf("In the cloudflared ingress rules add: hostname %s, service %s.", host, target)
	case "pangolin":
		return fmt.Sprintf("In Pangolin, add a resource for %s with the target %s (newt joins the same network).", host, target)
	case "tailscale":
		return fmt.Sprintf("Reach it through your tailnet at %s.", target)
	}
	return fmt.Sprintf("Point %s at %s.", host, target)
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
		for _, volume := range service.Volumes {
			path := BindSource(storage, plan.App.Name, volume.Name)
			write("    mkdir -p %s", path)
			if storage.Owner != "" {
				write("    sudo chown %s %s", storage.Owner, path)
			}
		}
	}
	write("")
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
		write("- %s for %s (%s): %s", need.Capability, need.Service, state, singleLine(need.Reason))
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

func publishedHostPort(service *ir.Service) int {
	for _, port := range service.Ports {
		if port.Publish {
			return rules.HostPort(port)
		}
	}
	return 0
}
