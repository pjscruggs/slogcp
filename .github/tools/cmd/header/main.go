// Copyright 2025-2026 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command header checks and repairs configured license headers through the
// SkyWalking Eyes header API.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/apache/skywalking-eyes/pkg/comments"
	"github.com/apache/skywalking-eyes/pkg/header"
	"github.com/apache/skywalking-eyes/pkg/logger"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	Header *headerConfig `yaml:"header"`
}

type headerConfig struct {
	License                  *configuredLicense           `yaml:"license"`
	Paths                    []string                     `yaml:"paths"`
	PathsIgnore              []string                     `yaml:"paths-ignore"`
	Comment                  header.CommentOption         `yaml:"comment"`
	LicenseLocationThreshold int                          `yaml:"license-location-threshold"`
	Languages                map[string]comments.Language `yaml:"language"`
}

type configuredLicense struct {
	header.LicenseConfig `yaml:",inline"`
	// Retained for YAML aliases in the repository's license configuration.
	FirstPublicationYear string `yaml:"first-publication-year"`
}

func decodeConfig(data []byte) (header.ConfigHeader, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var parsed fileConfig
	if err := decoder.Decode(&parsed); err != nil {
		return header.ConfigHeader{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return header.ConfigHeader{}, errors.New("configuration must contain a single YAML document")
		}
		return header.ConfigHeader{}, err
	}
	if parsed.Header == nil {
		return header.ConfigHeader{}, errors.New("configuration must contain a header mapping")
	}
	if parsed.Header.License == nil {
		return header.ConfigHeader{}, errors.New("configuration must contain a header.license mapping")
	}
	config := header.ConfigHeader{
		License:                  parsed.Header.License.LicenseConfig,
		Paths:                    parsed.Header.Paths,
		PathsIgnore:              parsed.Header.PathsIgnore,
		Comment:                  parsed.Header.Comment,
		LicenseLocationThreshold: parsed.Header.LicenseLocationThreshold,
		Languages:                parsed.Header.Languages,
	}
	if err := config.Finalize(); err != nil {
		return header.ConfigHeader{}, err
	}
	if strings.TrimSpace(config.GetLicenseContent()) == "" {
		return header.ConfigHeader{}, errors.New("configuration must resolve to a nonempty license header")
	}
	return config, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	logger.Log.SetLevel(logrus.InfoLevel)
	if len(args) < 2 || args[0] != "header" {
		return errors.New("usage: header header (check|fix) [-c config] [paths...]")
	}
	action := args[1]
	if action != "check" && action != "fix" {
		return fmt.Errorf("unsupported header action %q", action)
	}

	configPath := ".licenserc.yaml"
	var paths []string
	for i := 2; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "--config" {
			i++
			if i >= len(args) {
				return errors.New("configuration flag requires a path")
			}
			configPath = args[i]
			continue
		}
		if len(args[i]) > 0 && args[i][0] == '-' {
			return fmt.Errorf("unsupported flag %q", args[i])
		}
		paths = append(paths, args[i])
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	config, err := decodeConfig(data)
	if err != nil {
		return err
	}
	if len(paths) != 0 {
		config.Paths = paths
	}

	var result header.Result
	if err := header.Check(&config, &result); err != nil {
		return err
	}
	if action == "fix" {
		var fixErrors []error
		for _, file := range result.Failure {
			if err := header.Fix(file, &config, &result); err != nil {
				fixErrors = append(fixErrors, err)
			}
		}
		fmt.Println(result.String())
		return errors.Join(fixErrors...)
	}

	fmt.Println(result.String())
	if result.HasFailure() {
		return result.Error()
	}
	return nil
}
