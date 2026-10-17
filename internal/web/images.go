package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	serviceField    = "service"
	tagField        = "tag"
	builtLayout     = "2 Jan 2006"
	imageVersionRow = "image version"
	factSeparator   = " · "
	editedTagHint   = "change the image tag on the Files page so the edit is kept"
)

// imageTones colour what a check found.
var imageTones = map[string]string{
	pipeline.ImageUnchanged: toneOk,
	pipeline.ImageMoved:     toneWarn,
	pipeline.ImageUnpinned:  toneNeutral,
	pipeline.ImageUnchecked: toneNeutral,
}

// editedTagText says why a hand-edited compose.yaml takes no tag from here.
var editedTagText = capitalised(pipeline.ErrEditedCompose.Error()) + "; " + editedTagHint + "."

// tagProblems are the person's to fix: a tag or service the plan cannot take.
var tagProblems = []error{pipeline.ErrInvalidTag, pipeline.ErrUnknownService}

type imageRow struct {
	Service      string
	Pinned       string
	PinnedDigest string
	Now          string
	Digest       string
	Tone         string
	Newer        string
	// Offer is the tag the row offers to use; EditedHint says instead why
	// none is offered over a hand-edited compose.yaml.
	Offer      string
	EditedHint string
	Built      string
	Version    string
	Problem    string
}

func imageRows(statuses []pipeline.ImageStatus, edited bool) []imageRow {
	rows := make([]imageRow, 0, len(statuses))
	for _, status := range statuses {
		row := imageRow{Service: status.Service, Pinned: status.Image + ir.TagSeparator + status.Tag, PinnedDigest: pipeline.ShortDigest(status.Pinned), Now: pipeline.ImageStateTexts[status.State],
			Tone: imageTones[status.State], Digest: pipeline.ShortDigest(status.Current), Newer: status.NewerText(), Built: builtText(status.Created), Version: status.Version, Problem: status.Problem}
		offered := status.Newest != ""
		if offered && edited {
			row.EditedHint = editedTagText
		}
		if offered && !edited {
			row.Offer = status.Newest
		}
		rows = append(rows, row)
	}
	return rows
}

func builtText(created time.Time) string {
	if created.IsZero() {
		return ""
	}
	return created.Local().Format(builtLayout)
}

// checkImages asks the registries about the project's images again and
// shows what changed on the project page.
func (server *Server) checkImages(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	connection, err := server.connectTrusting(ctx, current.user.Username, request.PathValue("name"))
	if err != nil {
		return err
	}
	statuses, err := pipeline.CheckImages(ctx, connection.Dependencies, request.PathValue("name"))
	if err != nil {
		return err
	}
	return server.renderProject(writer, request, current, statuses)
}

// connectTrusting connects with the registries the project trusts beyond
// the configuration, so the egress guard lets its images be looked up.
func (server *Server) connectTrusting(ctx context.Context, username, name string) (pipeline.Connection, error) {
	stored, err := pipeline.Load(ctx, server.as(username), name)
	if err != nil {
		return pipeline.Connection{}, err
	}
	setup := server.pipelineSetup(username, pipeline.EngineRules, false)
	setup.ExtraRegistries = stored.Options.AllowRegistries
	return server.options.Connect(ctx, setup)
}

// useTag makes a version that runs a newer tag and tests it.
func (server *Server) useTag(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	name := request.PathValue("name")
	if err := server.busy(name); err != nil {
		return err
	}
	connection, err := server.connectTrusting(ctx, current.user.Username, name)
	if err != nil {
		return err
	}
	tag := strings.TrimSpace(request.PostFormValue(tagField))
	_, err = pipeline.UseTag(ctx, connection.Dependencies, name, request.PostFormValue(serviceField), tag)
	if errors.Is(err, pipeline.ErrEditedCompose) {
		return badRequest("%s", editedTagText)
	}
	if slices.ContainsFunc(tagProblems, func(problem error) bool { return errors.Is(err, problem) }) {
		return badRequest("%s.", capitalised(err.Error()))
	}
	if err != nil {
		return err
	}
	return server.startValidation(writer, request, current, name)
}

// imageFacts describe each service's image as the registry did when the
// version was made: version, build date, revision and source.
func imageFacts(recorded []store.Image) map[string]string {
	facts := map[string]string{}
	for _, image := range recorded {
		var parts []string
		for _, part := range []string{image.Labels[images.VersionLabel], builtText(image.Created), image.Labels[images.RevisionLabel], image.OCISource} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			facts[image.Service] = strings.Join(parts, factSeparator)
		}
	}
	return facts
}

// imageVersionRows are the version labels of a version's images.
func imageVersionRows(recorded []store.Image) []factRow {
	var rows []factRow
	for _, image := range recorded {
		if version := image.Labels[images.VersionLabel]; version != "" {
			rows = append(rows, factRow{Kind: imageVersionRow, Value: version, Source: image.Reference})
		}
	}
	return rows
}
