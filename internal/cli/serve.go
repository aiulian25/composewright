package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/web"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 2 * time.Minute
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
	stopTimeout       = 10 * time.Second
	// noSessionKept makes SetPassword end every session of the account.
	noSessionKept = ""
)

func (application *app) serveCommand() *cobra.Command {
	var listen string
	var trustedProxies []string
	var initialise bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the web UI (on 127.0.0.1:8484 by default)",
		Long: fmt.Sprintf(`Serve the web UI: a guided flow from a repository to a validated compose.yaml
and .env, the projects, and the settings. It listens on 127.0.0.1 only unless
you say otherwise, always asks for a login, and contacts nothing beyond what
the configuration allows.

On first start no administrator exists: open the UI within %d minutes and
create the account there. Later, restart serve, or use the setup link it
prints, which only someone who can read its output has. Creating the account
prints a warning with the address it came from, so a stranger's claim shows
up where the link does. With --init it first creates the data directory,
master key and config that are missing and keeps those that exist, as the
container image does on every start.

To reach it from other machines, listen on 0.0.0.0 and open
http://<this machine's address>:8484, or any name that leads to it, such as a
reverse proxy's. Over plain HTTP the password crosses the network
unencrypted, which is fine on a network you trust. The UI controls Docker,
which is root-equivalent on this host: beyond a trusted network, put HTTPS
in front, and the session cookie becomes Secure by itself.

Behind a reverse proxy, also pass its address with --trusted-proxy, so failed
sign-ins count against each visitor named in X-Forwarded-For rather than
against the proxy, which would lock everyone out. Anything at a trusted
address can name any visitor, so trust the proxy itself, not a whole network.`, int(web.SetupWindow.Minutes())),
		Example: "  composewright serve\n  composewright serve --listen 0.0.0.0:8484\n  composewright serve --listen 0.0.0.0:8484 --trusted-proxy 172.18.0.5",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return application.runServe(command.Context(), listen, trustedProxies, initialise)
		},
	}
	command.Flags().StringVar(&listen, "listen", "", "address to listen on (default: listen in config.yaml)")
	command.Flags().StringSlice(allowHostFlag, nil, "ignored: every host name is answered")
	if err := command.Flags().MarkDeprecated(allowHostFlag, "every host name is answered now; remove the flag"); err != nil {
		panic(err)
	}
	command.Flags().StringSliceVar(&trustedProxies, "trusted-proxy", nil, "a reverse proxy's address or range, such as 172.18.0.5 or 172.18.0.0/24, whose X-Forwarded-For names the visitor (repeatable)")
	command.Flags().BoolVar(&initialise, "init", false, "first create the data directory, master key and config that are missing, like init")
	return command
}

func (application *app) passwordCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "password",
		Short: "Set a new password for the web UI's administrator",
		Long: `Set a new password for the web UI's administrator, for when it is forgotten.
composewright sends no email, so this runs on the machine where composewright runs,
as the same user and with the same --config and --data-dir. It asks for the
password twice without showing it (piped input is read once), prints the
username, and signs every browser out. The change is written to the audit log.`,
		Example: "  composewright password\n  docker exec -it composewright composewright password",
		Args:    cobra.NoArgs,
		RunE:    func(command *cobra.Command, _ []string) error { return application.runPassword(command.Context()) },
	}
}

func (application *app) runPassword(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	administrator, err := workspace.Store.Administrator(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("there is no administrator yet: open the web UI and create one")
	}
	if err != nil {
		return err
	}
	password, err := application.readNewPassword(administrator.Username)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := workspace.Store.SetPassword(ctx, administrator.ID, hash, noSessionKept); err != nil {
		return err
	}
	details := map[string]any{"user": administrator.Username, "all_sessions_ended": true}
	if err := workspace.Store.Audit(ctx, workspace.Actor, store.ActionPassword, "", details); err != nil {
		return err
	}
	if !application.global.json {
		fmt.Fprintf(application.stdout, "New password set for %s. Every browser is signed out; sign in again with it.\n", administrator.Username)
	}
	return nil
}

// readNewPassword asks twice on a terminal, so a typo cannot lock the
// administrator out again; piped input is read once.
func (application *app) readNewPassword(username string) (string, error) {
	password, err := application.readSecret("New password for " + username + ": ")
	if err != nil {
		return "", err
	}
	if err := auth.CheckPassword(password); err != nil {
		return "", err
	}
	if _, isTerminal := application.terminal(); !isTerminal {
		return password, nil
	}
	again, err := application.readSecret("The same password again: ")
	if err != nil {
		return "", err
	}
	if again != password {
		return "", errors.New("the two passwords differ; nothing was changed")
	}
	return password, nil
}

// proxyPrefixes reads --trusted-proxy: an address range, or one address
// standing for itself.
func proxyPrefixes(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := proxyPrefix(value)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func proxyPrefix(value string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix, nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("--trusted-proxy %q is not an address or range such as 172.18.0.5 or 172.18.0.0/24", value)
	}
	address = address.Unmap()
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func (application *app) runServe(ctx context.Context, listen string, trustedProxies []string, initialise bool) error {
	proxies, err := proxyPrefixes(trustedProxies)
	if err != nil {
		return err
	}
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	if err := initialiseFirst(ctx, initialise, loaded); err != nil {
		return err
	}
	if !loaded.Config.UI.AuthRequired {
		return errors.New("ui.auth_required must stay true: the web UI controls Docker, which is root-equivalent")
	}
	opened, err := workspace.Open(ctx, loaded)
	if err != nil {
		return err
	}
	defer opened.Close()
	if listen == "" {
		listen = loaded.Config.Listen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", listen, err)
	}
	token, err := application.setupToken(ctx, opened)
	if err != nil {
		return err
	}
	server, err := web.New(web.Options{Loaded: loaded, Flags: config.Flags{ConfigFile: application.global.configFile, DataDir: application.global.dataDir},
		Workspace: opened, Version: application.build.Version, Listen: listen, TrustedProxies: proxies, SetupToken: token,
		Connect: pipeline.Connect, Logger: application.logger})
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, IdleTimeout: idleTimeout,
		MaxHeaderBytes: maxHeaderBytes, ErrorLog: slog.NewLogLogger(application.logger.Handler(), slog.LevelWarn)}
	application.announce(host, port, token)
	failed := make(chan error, 1)
	go func() { failed <- httpServer.Serve(listener) }()
	select {
	case err := <-failed:
		server.Close()
		return err
	case <-ctx.Done():
	}
	fmt.Fprintln(application.stderr, "stopping: ending runs and removing their sandboxes…")
	server.Close()
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return httpServer.Shutdown(stopCtx)
}

// setupToken is the one-time secret in the setup link, made only while no
// administrator exists.
func (application *app) setupToken(ctx context.Context, opened *workspace.Workspace) (string, error) {
	exists, err := opened.Store.HasUsers(ctx)
	if err != nil || exists {
		return "", err
	}
	return auth.NewCSRF()
}

func (application *app) announce(host, port, token string) {
	address := "http://" + net.JoinHostPort(host, port)
	wildcard := host == "" || host == "0.0.0.0" || host == "::"
	if wildcard {
		address = "http://<this machine's address>:" + port
	}
	fmt.Fprintf(application.stdout, "composewright is running at %s\n", address)
	loopback := net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if !loopback && host != "localhost" {
		fmt.Fprintln(application.stderr, application.colours.warn("note:"), "anyone on this network can open the sign-in page of a UI that controls Docker; put HTTPS in front beyond a network you trust.")
	}
	if token != "" {
		fmt.Fprintf(application.stdout, "no administrator yet: open it within %d minutes to create one; after that, restart, or add /setup?token=%s to the address\n",
			int(web.SetupWindow.Minutes()), token)
	}
}

// initialiseFirst runs init before serving when asked; init keeps whatever
// already exists, so the container can ask on every start.
func initialiseFirst(ctx context.Context, wanted bool, loaded *config.Loaded) error {
	if !wanted {
		return nil
	}
	_, err := workspace.Init(ctx, loaded)
	return err
}
