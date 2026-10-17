package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
)

const noneShown = "-"

func (application *app) imagesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "images <project>",
		Short: "Check a project's images for a moved tag or newer tags",
		Long: `Ask each image's registry again what its tag points at, with HEAD requests
only (nothing is pulled), and list the newer stable tags of the same family:
for 2.2.3, the newer 2.2.x and 2.x tags. A tag that now points at a different
image than the one pinned is reported as moved. Images outside the registry
allowlist are not contacted. The check is written to the audit log.`,
		Example: "  composewright images miniflux\n  composewright images miniflux --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runImages(command.Context(), arguments[0])
		},
	}
}

func (application *app) runImages(ctx context.Context, name string) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	opened, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer opened.Close()
	stored, err := pipeline.Load(ctx, opened, name)
	if err != nil {
		return err
	}
	connection, err := pipeline.Connect(ctx, pipeline.Setup{Config: loaded.Config, Paths: loaded.Paths, Workspace: opened,
		Version: application.build.Version, Engine: pipeline.EngineRules, ExtraRegistries: stored.Options.AllowRegistries})
	if err != nil {
		return err
	}
	statuses, err := pipeline.CheckImages(ctx, connection.Dependencies, name)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, statuses)
	}
	rows := [][]string{{"SERVICE", "PINNED", "NOW", "NEWER", "BUILT", "VERSION"}}
	for _, status := range statuses {
		rows = append(rows, []string{sanitize(status.Service), imagePinned(status), imageNow(status),
			orNone(status.NewerText()), orNone(builtDate(status)), orNone(status.Version)})
	}
	return table(application.stdout, rows...)
}

func imagePinned(status pipeline.ImageStatus) string {
	text := sanitize(status.Image + ir.TagSeparator + status.Tag)
	if status.Pinned != "" {
		text += " " + pipeline.ShortDigest(status.Pinned)
	}
	return text
}

func imageNow(status pipeline.ImageStatus) string {
	text := pipeline.ImageStateTexts[status.State]
	if status.Current != "" {
		text += " " + pipeline.ShortDigest(status.Current)
	}
	if status.Problem != "" {
		text += ": " + sanitize(status.Problem)
	}
	return text
}

func builtDate(status pipeline.ImageStatus) string {
	if status.Created.IsZero() {
		return ""
	}
	return status.Created.Local().Format(time.DateOnly)
}

func orNone(text string) string {
	if text == "" {
		return noneShown
	}
	return sanitize(text)
}
