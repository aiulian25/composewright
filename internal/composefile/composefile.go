// Package composefile loads compose files that are not trusted: upstream
// files from a repository and files composewright rendered itself. Includes,
// extends, env_file and label_file are never followed, so loading a file can
// never read anything else from disk.
package composefile

import (
	"context"
	"io"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"
)

const projectName = "composewright-check"

// compose-go reports deprecations through logrus; they are noise here and a
// log line could echo values from an untrusted file.
func init() {
	logrus.SetOutput(io.Discard)
}

// Load parses content into a project. environment supplies interpolation
// values; with interpolate false, ${VAR} references are kept verbatim.
func Load(ctx context.Context, filename string, content []byte, environment map[string]string, interpolate bool) (*types.Project, error) {
	return loader.LoadWithContext(ctx, details(filename, content, environment), options(interpolate))
}

// LoadModel returns the raw mapping after parsing, before normalisation, which
// is what template.ExtractVariables expects.
func LoadModel(ctx context.Context, filename string, content []byte) (map[string]any, error) {
	return loader.LoadModelWithContext(ctx, details(filename, content, nil), options(false))
}

func details(filename string, content []byte, environment map[string]string) types.ConfigDetails {
	if environment == nil {
		environment = map[string]string{}
	}
	return types.ConfigDetails{
		WorkingDir:  "/nonexistent",
		ConfigFiles: []types.ConfigFile{{Filename: filename, Content: content}},
		Environment: environment,
	}
}

func options(interpolate bool) func(*loader.Options) {
	return func(options *loader.Options) {
		options.SetProjectName(projectName, true)
		options.SkipInclude = true
		options.SkipExtends = true
		options.SkipResolveEnvironment = true
		options.SkipResolveLabels = true
		options.SkipConsistencyCheck = true
		options.SkipInterpolation = !interpolate
		options.ResolvePaths = false
		loader.WithDiscardEnvFiles(options)
	}
}
