package extract

import (
	"strings"

	"github.com/aiulian25/composewright/internal/fetch"
)

// codeMentions finds environment variable reads in source code. They carry
// no description, so they rank lowest.
func codeMentions(relative string, content []byte) []Mention {
	var mentions []Mention
	for index, line := range strings.Split(string(content), "\n") {
		for _, match := range fetch.EnvReference.FindAllStringSubmatch(line, -1) {
			name := firstGroup(match)
			if name == "" {
				continue
			}
			mentions = append(mentions, Mention{Name: name, Kind: KindCode, Location: Location{File: relative, Line: index + 1}})
		}
	}
	return mentions
}

func firstGroup(match []string) string {
	for _, group := range match[1:] {
		if group != "" {
			return group
		}
	}
	return ""
}

// workflowImages finds published image names in CI workflows, for example
// the images list of docker/metadata-action.
func workflowImages(relative string, content []byte) []ImageMention {
	var images []ImageMention
	for index, line := range strings.Split(string(content), "\n") {
		for _, match := range imageInText.FindAllStringSubmatch(line, -1) {
			images = append(images, ImageMention{Reference: match[1], Kind: KindWorkflow, Location: Location{File: relative, Line: index + 1}})
		}
	}
	return images
}
