package config

import (
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
)

// Paths is the single answer to "where do things live", the same in the
// container and in a local build.
type Paths struct {
	ConfigFile string
	DataDir    string
	KeyFile    string
}

type Flags struct {
	ConfigFile string
	DataDir    string
}

func (paths Paths) Database() string    { return filepath.Join(paths.DataDir, DatabaseName) }
func (paths Paths) KeysDir() string     { return filepath.Join(paths.DataDir, KeysDirName) }
func (paths Paths) ProjectsDir() string { return filepath.Join(paths.DataDir, ProjectsDir) }
func (paths Paths) BackupsDir() string  { return filepath.Join(paths.DataDir, BackupsDir) }

// DefaultKeyFile reports whether the master key lives at its default location
// inside the data directory, where the directory permissions are also ours.
func (paths Paths) DefaultKeyFile() bool {
	return paths.KeyFile == filepath.Join(paths.KeysDir(), KeyFileName)
}

func resolveConfigFile(flag string) string {
	explicit := firstNonEmpty(flag, os.Getenv(EnvConfig))
	if explicit != "" {
		return explicit
	}
	xdg.Reload()
	return filepath.Join(xdg.ConfigHome, AppName, ConfigFileName)
}

func resolvePaths(flags Flags, configFile string, fromFile Config) Paths {
	xdg.Reload()
	dataDir := firstNonEmpty(
		flags.DataDir,
		os.Getenv(EnvDataDir),
		fromFile.DataDir,
		filepath.Join(xdg.DataHome, AppName),
	)
	paths := Paths{ConfigFile: configFile, DataDir: dataDir}
	paths.KeyFile = firstNonEmpty(
		os.Getenv(EnvMasterKeyFile),
		fromFile.MasterKeyFile,
		filepath.Join(paths.KeysDir(), KeyFileName),
	)
	return paths
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
