package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

// What a check found about a service's pinned image.
const (
	ImageUnchanged = "unchanged"
	ImageMoved     = "moved"
	ImageUnpinned  = "unpinned"
	ImageUnchecked = "unchecked"
)

// ImageStateTexts say in words what a check found.
var ImageStateTexts = map[string]string{
	ImageUnchanged: "same image as pinned",
	ImageMoved:     "moved since it was pinned",
	ImageUnpinned:  "not pinned before",
	ImageUnchecked: "not checked",
}

const (
	outsideAllowlist = "the image is " + policy.OutsideAllowlist
	noImage          = "no image to look up"
	// NewerTagsShown is how many of the newest tags a check lists by name.
	NewerTagsShown = 5
	newerSeparator = ", "
	olderTagsText  = " and %d older"
	digestPrefix   = "sha256:"
	digestHexShown = 12
	digestShown    = len(digestPrefix) + digestHexShown
)

var (
	ErrImagesOffline = errors.New("checking images needs the registries, and this run is offline")
	ErrEditedCompose = errors.New("compose.yaml was edited by hand, so a new tag would replace the edit")
)

// ShortDigest is the start of a digest, enough to tell two images apart.
func ShortDigest(digest string) string {
	return digest[:min(len(digest), digestShown)]
}

// ImageStatus is what the registry says now about a service's image next to
// what was pinned when the latest version was made.
type ImageStatus struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Tag     string `json:"tag"`
	Pinned  string `json:"pinned_digest"`
	Current string `json:"current_digest"`
	State   string `json:"state"`
	// Newer are stable tags of the same family newer than Tag, oldest
	// first; Newest is the last of them.
	Newer   []string  `json:"newer"`
	Newest  string    `json:"newest,omitempty"`
	Version string    `json:"version"`
	Created time.Time `json:"created,omitzero"`
	Problem string    `json:"problem,omitempty"`
}

// NewerText lists the newest NewerTagsShown of Newer and counts the rest.
func (status ImageStatus) NewerText() string {
	older := max(len(status.Newer)-NewerTagsShown, 0)
	text := strings.Join(status.Newer[older:], newerSeparator)
	if older > 0 {
		text += fmt.Sprintf(olderTagsText, older)
	}
	return text
}

// CheckImages resolves every service's tag again, past the cache and with
// HEAD requests only, and lists the newer tags of its family. Images outside
// the allowlist are not contacted. The check is audited.
func CheckImages(ctx context.Context, dependencies Dependencies, name string) ([]ImageStatus, error) {
	inspector := dependencies.Inspector
	if inspector == nil {
		return nil, ErrImagesOffline
	}
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	recorded := map[string]store.Image{}
	for _, image := range stored.Images {
		recorded[image.Service] = image
	}
	running, err := runningImages(ctx, stored)
	if err != nil {
		return nil, err
	}
	input := policyInput(ctx, dependencies, stored.Plan, stored.source(), stored.Options)
	statuses := make([]ImageStatus, 0, len(stored.Plan.Services))
	moved, updates := []string{}, map[string]string{}
	for _, service := range stored.Plan.Services {
		image := running[service.Name]
		status := ImageStatus{Service: service.Name, Image: image.Reference(), Tag: image.Tag, Pinned: image.Digest, State: ImageUnchecked}
		if image.TaggedReference() == service.Image.TaggedReference() {
			facts := recorded[service.Name]
			status.Version, status.Created = facts.Labels[images.VersionLabel], facts.Created
		}
		if image.Registry == "" || image.Repository == "" {
			status.Problem = noImage
			statuses = append(statuses, status)
			continue
		}
		if !policy.RegistryAllowed(image.Reference(), input.AllowRegistries, input.ExtraRegistries, input.RepositoryOwner) {
			status.Problem = outsideAllowlist
			statuses = append(statuses, status)
			continue
		}
		status.Current, err = inspector.Digest(ctx, image)
		if err != nil {
			status.Problem = err.Error()
			statuses = append(statuses, status)
			continue
		}
		status.State = digestState(status.Pinned, status.Current)
		if status.State == ImageMoved {
			moved = append(moved, service.Name)
		}
		if !images.Versioned(image.Tag) {
			statuses = append(statuses, status)
			continue
		}
		tags, err := inspector.Tags(ctx, image)
		if err != nil {
			status.Problem = err.Error()
		}
		if status.Newer = images.Newer(image.Tag, tags); len(status.Newer) > 0 {
			status.Newest = status.Newer[len(status.Newer)-1]
			updates[service.Name] = status.Newest
		}
		statuses = append(statuses, status)
	}
	details := map[string]any{"generation": stored.Generation.ID, "moved": moved, "newest": updates}
	return statuses, dependencies.Workspace.Store.Audit(ctx, dependencies.Workspace.Actor, store.ActionCheckImages, stored.Project.ID, details)
}

// runningImages are the images the latest compose.yaml runs, by service: the
// plan's, or the ones written by hand when it was edited.
func runningImages(ctx context.Context, stored *Stored) (map[string]ir.Image, error) {
	running := map[string]ir.Image{}
	for _, service := range stored.Plan.Services {
		running[service.Name] = service.Image
	}
	if !stored.Options.Edited {
		return running, nil
	}
	project, err := composefile.Load(ctx, composeFileName, []byte(stored.Generation.ComposeYAML), nil, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrComposeInvalid, err)
	}
	for name := range running {
		// A service the edit removed, or an image that does not parse, is
		// the empty image, reported as nothing to look up.
		running[name], _ = ir.ParseImage(project.Services[name].Image)
	}
	return running, nil
}

func digestState(pinned, current string) string {
	if pinned == "" {
		return ImageUnpinned
	}
	if pinned != current {
		return ImageMoved
	}
	return ImageUnchanged
}

// UseTag stores a new version of a project whose service runs tag, with the
// latest version's settings; the image is verified, pinned and gated again
// and compose.yaml rendered again, so a hand-edited one is refused.
func UseTag(ctx context.Context, dependencies Dependencies, name, service, tag string) (*Result, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	if stored.Options.Edited {
		return nil, ErrEditedCompose
	}
	kept := stored.Options
	revision := Revision{ExposeVia: kept.ExposeVia, Networks: kept.Networks, Storage: kept.Storage, AllowNeeds: kept.AllowNeeds,
		AllowRegistries: kept.AllowRegistries, Images: map[string]string{service: tag}}
	return Revise(ctx, dependencies, name, revision)
}

// lostTagNotice says a tag a person chose was dropped because the
// regenerated plan has no service of that name any more.
const lostTagNotice = "the tag %s chosen for %s was not kept: the plan has no such service"

// withTags are the tags a person chose before, with those of a revision
// laid over them.
func withTags(kept, chosen map[string]string) map[string]string {
	if len(chosen) == 0 {
		return kept
	}
	tags := maps.Clone(kept)
	if tags == nil {
		tags = map[string]string{}
	}
	maps.Copy(tags, chosen)
	return tags
}

// keepChosenTags puts back on a generated plan the image tags a person
// chose with Use tag: those the options name, else those the project's
// latest version from the same source kept. The plan's images are verified
// and pinned afterwards, these with them.
func keepChosenTags(ctx context.Context, opened *workspace.Workspace, options *Options, result *Result) error {
	if options.ImageTags == nil && opened != nil {
		previous, err := previousTags(ctx, opened.Store, result.Plan.App.Name, result.Source.URL, options.Uploaded)
		if err != nil {
			return err
		}
		options.ImageTags = previous
	}
	chosen := ir.Provenance{Source: ir.SourceUser, SourceRef: reviewSource, Confidence: ir.ConfidenceHigh}
	for _, name := range slices.Sorted(maps.Keys(options.ImageTags)) {
		service, found := result.Plan.Service(name)
		if !found {
			result.Notices = append(result.Notices, fmt.Sprintf(lostTagNotice, options.ImageTags[name], name))
			continue
		}
		service.Image.Tag, service.Image.Digest, service.Image.Provenance = options.ImageTags[name], "", chosen
	}
	return nil
}

// previousTags are the tags the project's latest version kept, when it was
// made from the same source; an upload always counts as the same.
func previousTags(ctx context.Context, database *store.Store, name, sourceURL string, uploaded bool) (map[string]string, error) {
	project, generation, found, err := latestGeneration(ctx, database, name)
	if err != nil || !found {
		return nil, err
	}
	if !uploaded && project.SourceURL != sourceURL {
		return nil, nil
	}
	// A version whose options no longer read keeps no tags: a regeneration
	// is how such a version is mended.
	var kept Options
	_ = json.Unmarshal([]byte(generation.OptionsJSON), &kept)
	return kept.ImageTags, nil
}
