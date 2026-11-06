package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/doctor"
	"github.com/aiulian25/composewright/internal/store"
)

// HabitsLookup reads how the apps already running on the Docker host are
// set up.
type HabitsLookup func(ctx context.Context, configuration config.Config, projectsDir string) (doctor.Habits, error)

const (
	// habitsSetting records that the host's habits were followed, holding
	// what Home says about it until the person dismisses that.
	habitsSetting  = "learned_from_host"
	habitsActor    = "composewright:host-habits"
	habitsTimeout  = 30 * time.Second
	learnedJoiner  = " · "
	learnedZone    = "time zone %s, as %d of your apps have it"
	learnedNetwork = "new apps join %s, where %s reaches %d of your apps"
	keyTimeZone    = "profile.defaults.tz"
	keyNetworks    = "profile.networks"
)

// learnHabits sets, once per install, what the profile leaves open the way
// the apps already on the Docker host have it: their time zone, and the
// proxy or tunnel network they are reached through. A value a person set is
// never changed, and a host that does not answer is asked again at the
// next start.
func (server *Server) learnHabits() {
	lookup := server.options.Habits
	if lookup == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), habitsTimeout)
	defer cancel()
	habits, err := lookup(ctx, server.config(), server.paths().ProjectsDir())
	if err != nil {
		server.options.Logger.Info("the Docker host's habits could not be read; asking again at the next start", "error", err)
		return
	}
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	database := server.workspace().Store
	if _, learned, err := database.Setting(ctx, habitsSetting); err != nil || learned {
		return
	}
	values, learned := followHabits(server.config().Profile, habits)
	if err := server.applyHabits(ctx, values); err != nil {
		server.options.Logger.Warn("following the Docker host's habits", "error", err)
		return
	}
	if err := database.SetSetting(ctx, habitsSetting, strings.Join(learned, learnedJoiner)); err != nil {
		server.options.Logger.Warn("recording the Docker host's habits", "error", err)
	}
}

func (server *Server) applyHabits(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	if err := config.UpdateFile(server.paths().ConfigFile, values); err != nil {
		return err
	}
	if err := server.reload(); err != nil {
		return err
	}
	server.status.forget()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return server.workspace().Store.Audit(ctx, habitsActor, store.ActionSettings, "", map[string]any{"keys": keys})
}

// followHabits are the profile values the host's habits fill in where the
// profile leaves them open, with a line for each saying what was set.
func followHabits(profile config.Profile, habits doctor.Habits) (map[string]string, []string) {
	values := map[string]string{}
	var learned []string
	if profile.Defaults.TZ == "" && habits.TimeZone.Value != "" {
		values[keyTimeZone] = quote(habits.TimeZone.Value)
		learned = append(learned, fmt.Sprintf(learnedZone, habits.TimeZone.Value, habits.TimeZone.Apps))
	}
	if len(profile.Networks) == 0 && habits.Network.Network != "" {
		found := habits.Network
		network := config.ProfileNetwork{Name: found.Network, Kind: found.Kind, Provider: found.Provider, Default: true}
		values[keyNetworks] = networksJSON([]config.ProfileNetwork{network})
		learned = append(learned, fmt.Sprintf(learnedNetwork, found.Network, found.Proxy, habits.NetworkApps))
	}
	return values, learned
}

// learned is what Home says the host's habits set, until dismissed.
func (server *Server) learned(ctx context.Context) string {
	text, _, err := server.workspace().Store.Setting(ctx, habitsSetting)
	if err != nil {
		server.options.Logger.Warn("reading what the Docker host's habits set", "error", err)
	}
	return text
}

// dismissLearned takes the note about the host's habits off Home; they
// stay followed.
func (server *Server) dismissLearned(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	if err := server.workspace().Store.SetSetting(request.Context(), habitsSetting, ""); err != nil {
		return err
	}
	return redirect(writer, request, "/")
}
