// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package mage

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"
)

const packageSpecFile = "dev-tools/packaging/packages.yml"

// Packages defines the set of packages to be built when the package target is
// executed.
var Packages []OSPackageArgs

// UseElasticAgentCorePackaging configures the package target to build binary packages
// for an Elastic Agent.
func UseElasticAgentCorePackaging() {
	MustUsePackaging("elastic_agent_core", packageSpecFile)
}

// UseCommunityBeatPackaging configures the package target to build packages for
// a community Beat.
func UseCommunityBeatPackaging() {
	MustUsePackaging("community_beat", packageSpecFile)
}

// UseElasticAgentPackaging configures the package target to build packages for
// an Elastic Agent.
func UseElasticAgentPackaging() {
	// Prepare binaries so they can be packed into agent
	MustUsePackaging("elastic_beat_agent_binaries", packageSpecFile)
}

// UseElasticAgentDemoPackaging configures the package target to build packages for
// an Elastic Agent demo purposes.
func UseElasticAgentDemoPackaging() {
	// Prepare binaries so they can be packed into agent
	MustUsePackaging("elastic_beat_agent_demo_binaries", packageSpecFile)
}

// UseElasticBeatPackaging configures the package target to build packages for
// an Elastic Beat. This means it will generate two sets of packages -- one
// that is purely OSS under Apache 2.0 and one that is licensed under the
// Elastic License and may contain additional X-Pack features.
func UseElasticBeatPackaging() {
	UseElasticBeatOSSPackaging()
	MustUsePackaging("elastic_beat_xpack_separate_binaries", packageSpecFile)
}

// UseElasticBeatOSSPackaging configures the package target to build OSS
// packages.
func UseElasticBeatOSSPackaging() {
	MustUsePackaging("elastic_beat_oss", packageSpecFile)
}

// UseElasticBeatXPackPackaging configures the package target to build Elastic
// licensed (X-Pack) packages.
func UseElasticBeatXPackPackaging() {
	MustUsePackaging("elastic_beat_xpack", packageSpecFile)
}

// UseElasticBeatXPackReducedPackaging configures the package target to build Elastic
// licensed (X-Pack) packages for agent use.
func UseElasticBeatXPackReducedPackaging() {
	MustUsePackaging("elastic_beat_xpack_reduced", packageSpecFile)
}

// UseElasticBeatWithoutXPackPackaging configures the package target to build
// packages for an Elastic Beat. This means it will generate two sets of
// packages -- one that is purely OSS under Apache 2.0 and one that is licensed
// under the Elastic License and may contain additional X-Pack features.
//
// NOTE: This method doesn't use binaries produced in the x-pack folder, this is
// a temporary packaging target for projects that depends on beat but do have
// concrete x-pack binaries.
func UseElasticBeatWithoutXPackPackaging() {
	UseElasticBeatOSSPackaging()
	UseElasticBeatXPackPackaging()
}

// MustUsePackaging will load a named spec from a named file, if any errors
// occurs when loading the specs it will panic.
//
// NOTE: we assume that specFile is relative to the beatsDir.
func MustUsePackaging(specName, specFile string) {
	beatsDir, err := ElasticBeatsDir()
	if err != nil {
		panic(err)
	}

	err = LoadNamedSpec(specName, filepath.Join(beatsDir, specFile))
	if err != nil {
		panic(err)
	}
}

// LoadLocalNamedSpec loads the named package spec from the packages.yml in the
// current directory.
func LoadLocalNamedSpec(name string) {
	beatsDir, err := ElasticBeatsDir()
	if err != nil {
		panic(err)
	}

	err = LoadNamedSpec(name, filepath.Join(beatsDir, packageSpecFile), "packages.yml")
	if err != nil {
		panic(err)
	}
}

// LoadNamedSpec loads a packaging specification with the given name from the
// specified YAML file. name should be a sub-key of 'specs'.
func LoadNamedSpec(name string, files ...string) error {
	specs, err := LoadSpecs(files...)
	if err != nil {
		return fmt.Errorf("failed to load spec file: %w", err)
	}

	packages, found := specs[name]
	if !found {
		return fmt.Errorf("%v not found in package specs", name)
	}

	log.Printf("%v package spec loaded from %v", name, files)
	Packages = append(Packages, packages...)
	return nil
}

// LoadSpecs loads the packaging specifications from the specified YAML files.
func LoadSpecs(files ...string) (map[string][]OSPackageArgs, error) {
	var data [][]byte
	for _, file := range files {
		d, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read from spec file: %w", err)
		}
		data = append(data, d)
	}

	type PackageYAML struct {
		Specs map[string][]OSPackageArgs `yaml:"specs"`
	}

	var packages PackageYAML
	if err := yaml.Unmarshal(bytes.Join(data, []byte{'\n'}), &packages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec data: %w", err)
	}

	// verify that the package specification sets the docker variant
	for specName, specs := range packages.Specs {
		for _, spec := range specs {
			for _, pkgType := range spec.Types {
				if pkgType == Docker && spec.Spec.DockerVariant == Undefined {
					return nil, fmt.Errorf("%s defined a package spec for docker without a docker_variant set", specName)
				}
			}
		}
	}

	return packages.Specs, nil
}
