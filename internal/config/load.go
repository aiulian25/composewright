package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"go.yaml.in/yaml/v3"
)

type Loaded struct {
	Config     Config
	Paths      Paths
	FileExists bool
}

func Default() Config {
	config, err := Decode(DefaultTemplate)
	if err != nil {
		panic(fmt.Sprintf("embedded default config is invalid: %v", err))
	}
	return config
}

func Load(flags Flags) (*Loaded, error) {
	configFile := FilePath(flags.ConfigFile)
	content, err := os.ReadFile(configFile)
	fileExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read config %s: %w", configFile, err)
	}
	config := Default()
	if fileExists {
		config, err = decodeOnto(Default(), content)
		if err != nil {
			return nil, fmt.Errorf("config %s: %w", configFile, err)
		}
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", configFile, err)
	}
	return &Loaded{Config: config, Paths: resolvePaths(flags, configFile, config), FileExists: fileExists}, nil
}

// Decode parses a config document strictly: unknown keys are errors so a typo
// never silently falls back to a default.
func Decode(content []byte) (Config, error) {
	return decodeOnto(Config{}, content)
}

func decodeOnto(base Config, content []byte) (Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	err := decoder.Decode(&base)
	if errors.Is(err, io.EOF) {
		return base, nil
	}
	return base, err
}

func DecodeProfile(content []byte) (Profile, error) {
	var profile Profile
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	err := decoder.Decode(&profile)
	if errors.Is(err, io.EOF) {
		return profile, nil
	}
	return profile, err
}
