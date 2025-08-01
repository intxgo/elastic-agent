// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package mage

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"gopkg.in/yaml.v3"

	"github.com/elastic/elastic-agent/dev-tools/mage/pkgcommon"
	"github.com/elastic/elastic-agent/dev-tools/packaging"
)

const (
	// distributionsDir is the dir where packages are written.
	distributionsDir = "build/distributions"

	// packageStagingDir is the staging directory for any temporary files that
	// need to be written to disk for inclusion in a package.
	packageStagingDir = "build/package"

	// defaultBinaryName specifies the output file for zip and tar.gz.
	defaultBinaryName = "{{.Name}}{{if .Qualifier}}-{{.Qualifier}}{{end}}-{{.Version}}{{if .Snapshot}}-SNAPSHOT{{end}}{{if .OS}}-{{.OS}}{{end}}{{if .Arch}}-{{.Arch}}{{end}}"

	// defaultRootDir is the default name of the root directory contained inside of zip and
	// tar.gz packages.
	// NOTE: This uses .BeatName instead of .Name because we wanted the internal
	// directory to not include "-oss".
	defaultRootDir = "{{.BeatName}}{{if .Qualifier}}-{{.Qualifier}}{{end}}-{{.Version}}{{if .Snapshot}}-SNAPSHOT{{end}}{{if .OS}}-{{.OS}}{{end}}{{if .Arch}}-{{.Arch}}{{end}}"

	componentConfigMode os.FileMode = 0600

	rpm     = "rpm"
	deb     = "deb"
	zipExt  = "zip"
	targz   = "tar.gz"
	docker  = "docker"
	invalid = "invalid"
)

var (
	configFilePattern          = regexp.MustCompile(`.*\.yml$|.*\.yml\.disabled$`)
	componentConfigFilePattern = regexp.MustCompile(`.*beat\.spec\.yml$|.*beat\.yml$|apm-server\.yml$|apm-server\.spec\.yml$|elastic-agent\.yml$`)
)

// Alias for pkgcommon.PackageType. This type is moved to the pkgcommon to
// resolve circular dependency problems
type PackageType pkgcommon.PackageType

// List of possible package types.
var (
	RPM    PackageType = PackageType(pkgcommon.RPM)
	Deb                = PackageType(pkgcommon.Deb)
	Zip                = PackageType(pkgcommon.Zip)
	TarGz              = PackageType(pkgcommon.TarGz)
	Docker             = PackageType(pkgcommon.Docker)
)

// OSPackageArgs define a set of package types to build for an operating
// system using the contained PackageSpec.
type OSPackageArgs struct {
	OS    string        `yaml:"os"`
	Arch  string        `yaml:"arch,omitempty"`
	Types []PackageType `yaml:"types"`
	Spec  PackageSpec   `yaml:"spec"`
}

// PackageSpec specifies package metadata and the contents of the package.
type PackageSpec struct {
	Name                    string                 `yaml:"name,omitempty"`
	ServiceName             string                 `yaml:"service_name,omitempty"`
	OS                      string                 `yaml:"os,omitempty"`
	Arch                    string                 `yaml:"arch,omitempty"`
	Vendor                  string                 `yaml:"vendor,omitempty"`
	Snapshot                bool                   `yaml:"snapshot"`
	FIPS                    bool                   `yaml:"fips"`
	Version                 string                 `yaml:"version,omitempty"`
	License                 string                 `yaml:"license,omitempty"`
	URL                     string                 `yaml:"url,omitempty"`
	Description             string                 `yaml:"description,omitempty"`
	DockerVariant           DockerVariant          `yaml:"docker_variant,omitempty"`
	DockerImageNameTemplate string                 `yaml:"docker_image_name_template,omitempty"` // Optional: template of the docker image name
	PreInstallScript        string                 `yaml:"pre_install_script,omitempty"`
	PostInstallScript       string                 `yaml:"post_install_script,omitempty"`
	PostRmScript            string                 `yaml:"post_rm_script,omitempty"`
	Files                   map[string]PackageFile `yaml:"files"`
	Qualifier               string                 `yaml:"qualifier,omitempty"`   // Optional
	OutputFile              string                 `yaml:"output_file,omitempty"` // Optional
	ExtraVars               map[string]string      `yaml:"extra_vars,omitempty"`  // Optional
	ExtraTags               []string               `yaml:"extra_tags,omitempty"`  // Optional
	Components              []packaging.BinarySpec `yaml:"components"`            // Optional: Components required for this package

	evalContext            map[string]interface{}
	packageDir             string
	localPreInstallScript  string
	localPostInstallScript string
	localPostRmScript      string
}

// add new prop into package file called expand spc
// expand spec is checked during packaging and expands to multiple files
// if expand is not present file is copied normally

// PackageFile represents a file or directory within a package.
type PackageFile struct {
	Source        string                  `yaml:"source,omitempty"`   // Regular source file or directory.
	Content       string                  `yaml:"content,omitempty"`  // Inline template string.
	Template      string                  `yaml:"template,omitempty"` // Input template file.
	Target        string                  `yaml:"target,omitempty"`   // Target location in package. Relative paths are added to a package specific directory (e.g. metricbeat-7.0.0-linux-x86_64).
	Mode          os.FileMode             `yaml:"mode,omitempty"`     // Target mode for file. Does not apply when source is a directory.
	ConfigMode    os.FileMode             `yaml:"config_mode,omitempty"`
	Config        bool                    `yaml:"config"`                    // Mark file as config in the package (deb and rpm only).
	Modules       bool                    `yaml:"modules"`                   // Mark directory as directory with modules.
	Dep           func(PackageSpec) error `yaml:"-" hash:"-" json:"-"`       // Dependency to invoke during Evaluate.
	Owner         string                  `yaml:"owner,omitempty"`           // File Owner, for user and group name (rpm only).
	SkipOnMissing bool                    `yaml:"skip_on_missing,omitempty"` // Prevents build failure if the file is missing.
	Symlink       bool                    `yaml:"symlink"`                   // Symlink marks file as a symlink pointing from target to source.
	ExpandSpec    bool                    `yaml:"expand_spec,omitempty"`     // Optional
}

// OSArchNames defines the names of architectures for use in packages.
var OSArchNames = map[string]map[PackageType]map[string]string{
	"windows": {
		Zip: {
			"386":   "x86",
			"amd64": "x86_64",
		},
	},
	"darwin": {
		TarGz: {
			"386":   "x86",
			"amd64": "x86_64",
			"arm64": "aarch64",
			// "universal": "universal",
		},
	},
	"linux": {
		RPM: {
			"386":      "i686",
			"amd64":    "x86_64",
			"armv7":    "armhfp",
			"arm64":    "aarch64",
			"mipsle":   "mipsel",
			"mips64le": "mips64el",
			"ppc64":    "ppc64",
			"ppc64le":  "ppc64le",
			"s390x":    "s390x",
		},
		// https://www.debian.org/ports/
		Deb: {
			"386":      "i386",
			"amd64":    "amd64",
			"armv5":    "armel",
			"armv6":    "armel",
			"armv7":    "armhf",
			"arm64":    "arm64",
			"mips":     "mips",
			"mipsle":   "mipsel",
			"mips64le": "mips64el",
			"ppc64le":  "ppc64el",
			"s390x":    "s390x",
		},
		TarGz: {
			"386":      "x86",
			"amd64":    "x86_64",
			"armv5":    "armv5",
			"armv6":    "armv6",
			"armv7":    "armv7",
			"arm64":    "arm64",
			"mips":     "mips",
			"mipsle":   "mipsel",
			"mips64":   "mips64",
			"mips64le": "mips64el",
			"ppc64":    "ppc64",
			"ppc64le":  "ppc64le",
			"s390x":    "s390x",
		},
		Docker: {
			"amd64": "amd64",
			"arm64": "arm64",
		},
	},
	"aix": {
		TarGz: {
			"ppc64": "ppc64",
		},
	},
}

// getOSArchName returns the architecture name to use in a package.
func getOSArchName(platform BuildPlatform, t PackageType) (string, error) {
	names, found := OSArchNames[platform.GOOS()]
	if !found {
		return "", fmt.Errorf("arch names for os=%v are not defined",
			platform.GOOS())
	}

	archMap, found := names[t]
	if !found {
		return "", fmt.Errorf("arch names for %v on os=%v are not defined",
			t, platform.GOOS())
	}

	arch, found := archMap[platform.Arch()]
	if !found {
		return "", fmt.Errorf("arch name associated with %v for %v on "+
			"os=%v is not defined", platform.Arch(), t, platform.GOOS())
	}

	return arch, nil
}

// String returns the name of the package type.
func (typ PackageType) String() string {
	switch typ {
	case RPM:
		return rpm
	case Deb:
		return deb
	case Zip:
		return zipExt
	case TarGz:
		return targz
	case Docker:
		return docker
	default:
		return invalid
	}
}

// MarshalText returns the text representation of PackageType.
func (typ PackageType) MarshalText() ([]byte, error) {
	return []byte(typ.String()), nil
}

// UnmarshalText returns a PackageType based on the given text.
func (typ *PackageType) UnmarshalText(text []byte) error {
	switch strings.ToLower(string(text)) {
	case rpm:
		*typ = RPM
	case deb:
		*typ = Deb
	case targz, "tgz", "targz":
		*typ = TarGz
	case zipExt:
		*typ = Zip
	case docker:
		*typ = Docker
	default:
		return fmt.Errorf("unknown package type: %v", string(text))
	}
	return nil
}

// AddFileExtension returns a filename with the file extension added. If the
// filename already has the extension then it becomes a pass-through.
func (typ PackageType) AddFileExtension(file string) string {
	ext := "." + strings.ToLower(typ.String())
	if !strings.HasSuffix(file, ext) {
		return file + ext
	}
	return file
}

// PackagingDir returns the path that should be used for building and packaging.
// The path returned guarantees that packaging operations can run in isolation.
func (typ PackageType) PackagingDir(home string, target BuildPlatform, spec PackageSpec) (string, error) {
	root := home
	if typ == Docker {
		root = filepath.Join(root, spec.ImageName())
	}

	targetPath := typ.AddFileExtension(spec.Name + "-" + target.GOOS() + "-" + target.Arch())
	return filepath.Join(root, targetPath), nil
}

// Build builds a package based on the provided spec.
func (typ PackageType) Build(spec PackageSpec) error {
	switch typ {
	case RPM:
		return PackageRPM(spec)
	case Deb:
		return PackageDeb(spec)
	case Zip:
		return PackageZip(spec)
	case TarGz:
		return PackageTarGz(spec)
	case Docker:
		return PackageDocker(spec)
	default:
		return fmt.Errorf("unknown package type: %v", typ)
	}
}

// Clone returns a deep clone of the spec.
func (s PackageSpec) Clone() PackageSpec {
	clone := s
	clone.Files = make(map[string]PackageFile, len(s.Files))
	for k, v := range s.Files {
		clone.Files[k] = v
	}
	clone.ExtraVars = make(map[string]string, len(s.ExtraVars))
	for k, v := range s.ExtraVars {
		clone.ExtraVars[k] = v
	}
	return clone
}

// ReplaceFile replaces an existing file defined in the spec. The target must
// exist other it will panic.
func (s PackageSpec) ReplaceFile(target string, file PackageFile) {
	_, found := s.Files[target]
	if !found {
		panic(fmt.Errorf("failed to ReplaceFile because target=%v does not exist", target))
	}

	s.Files[target] = file
}

// ExtraVar adds or replaces a variable to `extra_vars` in package specs.
func (s *PackageSpec) ExtraVar(key, value string) {
	if s.ExtraVars == nil {
		s.ExtraVars = make(map[string]string)
	}
	s.ExtraVars[key] = value
}

// Expand expands a templated string using data from the spec.
func (s PackageSpec) Expand(in string, args ...map[string]interface{}) (string, error) {
	return expandTemplate("inline", in, FuncMap,
		EnvMap(append([]map[string]interface{}{s.evalContext, s.toMap()}, args...)...))
}

// MustExpand expands a templated string using data from the spec. It panics if
// an error occurs.
func (s PackageSpec) MustExpand(in string, args ...map[string]interface{}) string {
	v, err := s.Expand(in, args...)
	if err != nil {
		panic(err)
	}
	return v
}

// ExpandFile expands a template file using data from the spec.
func (s PackageSpec) ExpandFile(src, dst string, args ...map[string]interface{}) error {
	return expandFile(src, dst,
		EnvMap(append([]map[string]interface{}{s.evalContext, s.toMap()}, args...)...))
}

// MustExpandFile expands a template file using data from the spec. It panics if
// an error occurs.
func (s PackageSpec) MustExpandFile(src, dst string, args ...map[string]interface{}) {
	if err := s.ExpandFile(src, dst, args...); err != nil {
		panic(err)
	}
}

// Evaluate expands all variables used in the spec definition and writes any
// templated files used in the spec to disk. It panics if there is an error.
func (s PackageSpec) Evaluate(args ...map[string]interface{}) PackageSpec {
	args = append([]map[string]interface{}{s.toMap(), s.evalContext}, args...)
	mustExpand := func(in string) string {
		if in == "" {
			return ""
		}
		return MustExpand(in, args...)
	}

	if s.evalContext == nil {
		s.evalContext = map[string]interface{}{}
	}

	for k, v := range s.ExtraVars {
		s.evalContext[k] = mustExpand(v)
	}

	if s.ExtraTags != nil {
		for i, tag := range s.ExtraTags {
			s.ExtraTags[i] = mustExpand(tag)
		}
	}

	s.Name = mustExpand(s.Name)
	s.ServiceName = mustExpand(s.ServiceName)
	s.OS = mustExpand(s.OS)
	s.Arch = mustExpand(s.Arch)
	s.Vendor = mustExpand(s.Vendor)
	s.Version = mustExpand(s.Version)
	s.License = mustExpand(s.License)
	s.URL = mustExpand(s.URL)
	s.Description = mustExpand(s.Description)
	s.PreInstallScript = mustExpand(s.PreInstallScript)
	s.PostInstallScript = mustExpand(s.PostInstallScript)
	s.PostRmScript = mustExpand(s.PostRmScript)
	s.OutputFile = mustExpand(s.OutputFile)

	if s.ServiceName == "" {
		s.ServiceName = s.Name
	}

	if s.packageDir == "" {
		outputFileName := filepath.Base(s.OutputFile)

		if outputFileName != "." {
			s.packageDir = filepath.Join(packageStagingDir, outputFileName)
		} else {
			s.packageDir = filepath.Join(packageStagingDir, strings.Join([]string{s.Name, s.OS, s.Arch, s.hash()}, "-"))
		}
	} else {
		s.packageDir = filepath.Clean(mustExpand(s.packageDir))
	}
	s.evalContext["PackageDir"] = s.packageDir
	s.evalContext["fips"] = s.FIPS

	evaluatedFiles := make(map[string]PackageFile, len(s.Files))
	for target, f := range s.Files {
		// Execute the dependency if it exists.
		if f.Dep != nil {
			if err := f.Dep(s); err != nil {
				panic(fmt.Errorf("failed executing package file dependency for target=%v: %w", target, err))
			}
		}

		f.Source = s.MustExpand(f.Source)
		f.Template = s.MustExpand(f.Template)
		f.Target = s.MustExpand(target)
		target = f.Target

		// Expand templates.
		switch {
		case f.Source != "":
		case f.Content != "":
			content, err := s.Expand(f.Content)
			if err != nil {
				panic(fmt.Errorf("failed to expand content template for target=%v: %w", target, err))
			}

			f.Source = filepath.Join(s.packageDir, filepath.Base(f.Target))
			if err = os.WriteFile(CreateDir(f.Source), []byte(content), 0644); err != nil {
				panic(fmt.Errorf("failed to write file containing content for target=%v: %w", target, err))
			}
		case f.Template != "":
			f.Source = filepath.Join(s.packageDir, filepath.Base(f.Template))
			if err := s.ExpandFile(f.Template, CreateDir(f.Source)); err != nil {
				panic(fmt.Errorf("failed to expand template file for target=%v: %w", target, err))
			}
		default:
			panic(fmt.Errorf("package file with target=%v must have either source, content, or template", target))
		}

		evaluatedFiles[f.Target] = f
	}
	// Replace the map instead of modifying the source.
	s.Files = evaluatedFiles

	if err := copyInstallScript(s, s.PreInstallScript, &s.localPreInstallScript); err != nil {
		panic(err)
	}
	if err := copyInstallScript(s, s.PostInstallScript, &s.localPostInstallScript); err != nil {
		panic(err)
	}
	if err := copyInstallScript(s, s.PostRmScript, &s.localPostRmScript); err != nil {
		panic(err)
	}

	return s
}

// ImageName computes the image name from the spec.
func (s PackageSpec) ImageName() string {
	if s.DockerImageNameTemplate != "" {
		imageNameTmpl, err := template.New("dockerImageTemplate").Parse(s.DockerImageNameTemplate)
		if err != nil {
			panic(fmt.Errorf("parsing docker image name template for %s variant %s: %w", s.Name, s.DockerVariant, err))
		}

		data := s.toMap()
		for k, v := range varMap() {
			data[k] = v
		}

		buf := new(strings.Builder)
		err = imageNameTmpl.Execute(buf, data)
		if err != nil {
			panic(fmt.Errorf("rendering docker image name template for %s variant %s: %w", s.Name, s.DockerVariant, err))
		}

		imageName := buf.String()
		if mg.Verbose() {
			log.Printf("rendered image name for %s variant %s: %s", s.Name, s.DockerVariant, imageName)
		}
		return imageName
	}

	if s.DockerVariant == Basic {
		return s.Name
	}
	if s.DockerVariant == EdotCollector || s.DockerVariant == EdotCollectorWolfi {
		// no suffix for basic docker variant
		return s.Name
	}

	return fmt.Sprintf("%s-%s", s.Name, s.DockerVariant)
}

func copyInstallScript(spec PackageSpec, script string, local *string) error {
	if script == "" {
		return nil
	}

	*local = filepath.Join(spec.packageDir, "scripts", filepath.Base(script))
	if filepath.Ext(*local) == ".tmpl" {
		*local = strings.TrimSuffix(*local, ".tmpl")
	}

	if strings.HasSuffix(*local, "."+spec.Name) {
		*local = strings.TrimSuffix(*local, "."+spec.Name)
	}

	if err := spec.ExpandFile(script, createDir(*local)); err != nil {
		return fmt.Errorf("failed to copy install script to package dir: %w", err)
	}

	if err := os.Chmod(*local, 0755); err != nil {
		return fmt.Errorf("failed to chmod install script: %w", err)
	}

	return nil
}

func (s PackageSpec) hash() string {
	out, err := yaml.Marshal(s)
	if err != nil {
		panic(fmt.Errorf("failed to marshal spec: %w", err))
	}

	h := fnv.New64()
	h.Write(out)

	hash := strconv.FormatUint(h.Sum64(), 10)
	if len(hash) > 10 {
		hash = hash[0:10]
	}
	return hash
}

// toMap returns a map containing the exported field names and their values.
func (s PackageSpec) toMap() map[string]interface{} {
	out := make(map[string]interface{})
	v := reflect.ValueOf(s)
	typ := v.Type()

	for i := 0; i < v.NumField(); i++ {
		structField := typ.Field(i)
		if !structField.Anonymous && structField.PkgPath == "" {
			out[structField.Name] = v.Field(i).Interface()
		}
	}

	return out
}

// rootDir returns the name of the root directory contained inside of zip and
// tar.gz packages.
func (s PackageSpec) rootDir() string {
	if s.OutputFile != "" {
		return filepath.Base(s.OutputFile)
	}

	return s.MustExpand(defaultRootDir)
}

// PackageZip packages a zip file.
func PackageZip(spec PackageSpec) error {
	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	// Create a new zip archive.
	w := zip.NewWriter(buf)
	baseDir := spec.rootDir()

	// Add files to zip.
	for _, pkgFile := range spec.Files {
		if pkgFile.Symlink {
			// not supported on zip archives
			continue
		}

		if err := addFileToZip(w, baseDir, pkgFile); err != nil {
			p, _ := filepath.Abs(pkgFile.Source)
			return fmt.Errorf("failed adding file=%+v to zip: %w", p, err)
		}
	}

	if err := w.Close(); err != nil {
		return err
	}

	// Output the zip file.
	if spec.OutputFile == "" {
		outputZip, err := spec.Expand(defaultBinaryName + ".zip")
		if err != nil {
			return err
		}
		spec.OutputFile = filepath.Join(distributionsDir, outputZip)
	}
	spec.OutputFile = Zip.AddFileExtension(spec.OutputFile)

	// Write the zip file.
	if err := os.WriteFile(CreateDir(spec.OutputFile), buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write zip file: %w", err)
	}

	// Any packages beginning with "tmp-" are temporary by nature so don't have
	// them a .sha512 file.
	if strings.HasPrefix(filepath.Base(spec.OutputFile), "tmp-") {
		return nil
	}

	if err := CreateSHA512File(spec.OutputFile); err != nil {
		return fmt.Errorf("failed to create .sha512 file: %w", err)
	}
	return nil
}

// PackageTarGz packages a gzipped tar file.
func PackageTarGz(spec PackageSpec) error {
	baseDir := spec.rootDir()

	// Create the output file.
	if spec.OutputFile == "" {
		outputTarGz, err := spec.Expand(defaultBinaryName + ".tar.gz")
		if err != nil {
			return err
		}
		spec.OutputFile = filepath.Join(distributionsDir, outputTarGz)
	}
	spec.OutputFile = TarGz.AddFileExtension(spec.OutputFile)

	// Open the output file.
	log.Println("Creating output file at", spec.OutputFile)
	outFile, err := os.Create(CreateDir(spec.OutputFile))
	if err != nil {
		return err
	}
	defer func() {
		if err := outFile.Close(); err != nil {
			log.Printf("failed to close output file: %v", err)
		}
	}()

	// Create a gzip writer to our output file
	gzWriter := gzip.NewWriter(outFile)
	defer func() {
		if err := gzWriter.Close(); err != nil {
			log.Printf("failed to close gzip writer: %v", err)
		}
	}()

	// Create a new tar archive.
	w := tar.NewWriter(gzWriter)
	defer func() {
		if err := w.Close(); err != nil {
			log.Printf("failed to close tar writer: %v", err)
		}
	}()

	// // Replace the darwin-universal by darwin-x86_64 and darwin-arm64. Also
	// // keep the other files.
	// if spec.Name == "elastic-agent" && spec.OS == "darwin" && spec.Arch == "universal" {
	// 	newFiles := map[string]PackageFile{}
	// 	for filename, pkgFile := range spec.Files {
	// 		if strings.Contains(pkgFile.Target, "darwin-universal") &&
	// 			strings.Contains(pkgFile.Target, "downloads") {
	//
	// 			amdFilename, amdpkgFile := replaceFileArch(filename, pkgFile, "x86_64")
	// 			armFilename, armpkgFile := replaceFileArch(filename, pkgFile, "aarch64")
	//
	// 			newFiles[amdFilename] = amdpkgFile
	// 			newFiles[armFilename] = armpkgFile
	// 		} else {
	// 			newFiles[filename] = pkgFile
	// 		}
	// 	}
	//
	// 	spec.Files = newFiles
	// }

	// Add files to tar.
	for _, pkgFile := range spec.Files {
		if pkgFile.Symlink {
			continue
		}

		if err := addFileToTar(w, baseDir, pkgFile); err != nil {
			return fmt.Errorf("failed adding file=%+v to tar: %w", pkgFile, err)
		}
	}

	// same for symlinks so they can point to files in tar
	for _, pkgFile := range spec.Files {
		if !pkgFile.Symlink {
			continue
		}

		tmpdir, err := os.MkdirTemp("", "TmpSymlinkDropPath")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)

		if err := addSymlinkToTar(tmpdir, w, baseDir, pkgFile); err != nil {
			return fmt.Errorf("failed adding file=%+v to tar: %w", pkgFile, err)
		}
	}

	if err := w.Close(); err != nil {
		return err
	}
	if err := gzWriter.Close(); err != nil {
		return err
	}
	if err := outFile.Close(); err != nil {
		return err
	}

	// Any packages beginning with "tmp-" are temporary by nature so don't have
	// them a .sha512 file.
	if strings.HasPrefix(filepath.Base(spec.OutputFile), "tmp-") {
		return nil
	}

	if err := CreateSHA512File(spec.OutputFile); err != nil {
		return fmt.Errorf("failed to create .sha512 file: %w", err)
	}
	return nil
}

// PackageDeb packages a deb file. This requires Docker to execute FPM.
func PackageDeb(spec PackageSpec) error {
	return runFPM(spec, Deb)
}

// PackageRPM packages a RPM file. This requires Docker to execute FPM.
func PackageRPM(spec PackageSpec) error {
	return runFPM(spec, RPM)
}

func runFPM(spec PackageSpec, packageType PackageType) error {
	var fpmPackageType string
	switch packageType {
	case RPM, Deb:
		fpmPackageType = packageType.String()
	default:
		return fmt.Errorf("unsupported package type=%v for runFPM", fpmPackageType)
	}

	if err := HaveDocker(); err != nil {
		return fmt.Errorf("packaging %v files requires docker: %w", fpmPackageType, err)
	}

	// Build a tar file as the input to FPM.
	inputTar := filepath.Join(distributionsDir, "tmp-"+fpmPackageType+"-"+spec.rootDir()+"-"+spec.hash()+".tar.gz")
	spec.OutputFile = inputTar
	if err := PackageTarGz(spec); err != nil {
		return err
	}
	defer os.Remove(inputTar)

	outputFile, err := spec.Expand("{{.Name}}-{{.Version}}{{if .Snapshot}}-SNAPSHOT{{end}}-{{.Arch}}")
	if err != nil {
		return err
	}
	spec.OutputFile = packageType.AddFileExtension(filepath.Join(distributionsDir, outputFile))

	dockerRun := sh.RunCmd("docker", "run")
	var args []string

	args, err = addUIDGidEnvArgs(args)
	if err != nil {
		return err
	}

	args = append(args,
		"--rm",
		"-w", "/app",
		"-v", CWD()+":/app",
		beatsFPMImage+":"+fpmVersion,
		"fpm", "--force",
		"--input-type", "tar",
		"--output-type", fpmPackageType,
		"--name", spec.ServiceName,
		"--architecture", spec.Arch,
	)
	if packageType == RPM {
		args = append(args,
			"--rpm-rpmbuild-define", "_build_id_links none",
			"--rpm-digest", "sha256",
		)
	}
	if spec.Version != "" {
		args = append(args, "--version", spec.Version)
	}
	if spec.Vendor != "" {
		args = append(args, "--vendor", spec.Vendor)
	}
	if spec.License != "" {
		args = append(args, "--license", strings.ReplaceAll(spec.License, " ", "-"))
	}
	if spec.Description != "" {
		args = append(args, "--description", spec.Description)
	}
	if spec.URL != "" {
		args = append(args, "--url", spec.URL)
	}
	if spec.localPreInstallScript != "" {
		args = append(args, "--before-install", spec.localPreInstallScript)
	}
	if spec.localPostInstallScript != "" {
		args = append(args, "--after-install", spec.localPostInstallScript)
	}
	if spec.localPostRmScript != "" {
		args = append(args, "--after-remove", spec.localPostRmScript)
	}
	for _, pf := range spec.Files {
		if pf.Config {
			args = append(args, "--config-files", pf.Target)
		}
		if pf.Owner != "" {
			args = append(args, "--rpm-attr", fmt.Sprintf("%04o,%s,%s:%s", pf.Mode, pf.Owner, pf.Owner, pf.Target))
		}
	}
	args = append(args,
		"-p", spec.OutputFile,
		inputTar,
	)

	if err = dockerRun(args...); err != nil {
		return fmt.Errorf("failed while running FPM in docker: %w", err)
	}

	if err := CreateSHA512File(spec.OutputFile); err != nil {
		return fmt.Errorf("failed to create .sha512 file: %w", err)
	}
	return nil
}

func addUIDGidEnvArgs(args []string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return args, nil
	}

	info, err := GetDockerInfo()
	if err != nil {
		return args, fmt.Errorf("failed to get docker info: %w", err)
	}

	uid, gid := os.Getuid(), os.Getgid()
	if info.IsBoot2Docker() {
		// Boot2Docker mounts vboxfs using 1000:50.
		uid, gid = 1000, 50
		log.Printf("Boot2Docker is in use. Deploying workaround. "+
			"Using UID=%d GID=%d", uid, gid)
	}

	return append(args,
		"-e", "EXEC_UID="+strconv.Itoa(uid),
		"-e", "EXEC_GID="+strconv.Itoa(gid),
	), nil
}

// addFileToZip adds a file (or directory) to a zip archive.
func addFileToZip(ar *zip.Writer, baseDir string, pkgFile PackageFile) error {
	return filepath.Walk(pkgFile.Source, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if pkgFile.SkipOnMissing && os.IsNotExist(err) {
				return nil
			}

			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		switch {
		case componentConfigFilePattern.MatchString(info.Name()):
			header.SetMode(componentConfigMode & os.ModePerm)
		case pkgFile.ConfigMode > 0 && configFilePattern.MatchString(info.Name()):
			header.SetMode(pkgFile.ConfigMode & os.ModePerm)
		case info.Mode().IsRegular() && pkgFile.Mode > 0:
			header.SetMode(pkgFile.Mode & os.ModePerm)
		case info.IsDir():
			header.SetMode(0755)
		}

		if filepath.IsAbs(pkgFile.Target) {
			baseDir = ""
		}

		relPath, err := filepath.Rel(pkgFile.Source, path)
		if err != nil {
			return err
		}

		header.Name = filepath.Join(baseDir, pkgFile.Target, relPath)

		if info.IsDir() {
			header.Name += string(filepath.Separator)
		} else {
			header.Method = zip.Deflate
		}

		if mg.Verbose() {
			log.Println("Adding", header.Mode(), header.Name)
		}

		w, err := ar.CreateHeader(header)
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		if _, err = io.Copy(w, file); err != nil {
			return err
		}
		return file.Close()
	})
}

// addFileToTar adds a file (or directory) to a tar archive.
func addFileToTar(ar *tar.Writer, baseDir string, pkgFile PackageFile) error {
	excludedFiles := []string{}

	return filepath.WalkDir(pkgFile.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if pkgFile.SkipOnMissing && os.IsNotExist(err) {
				return nil
			}
			return err
		}

		if slices.Contains(excludedFiles, d.Name()) {
			// it's a file we have to exclude
			if mg.Verbose() {
				log.Printf("Skipping file %q...", path)
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}
		header.Uname, header.Gname = "root", "root"
		header.Uid, header.Gid = 0, 0

		switch {
		case componentConfigFilePattern.MatchString(info.Name()):
			header.Mode = int64(componentConfigMode & os.ModePerm)
		case pkgFile.ConfigMode > 0 && configFilePattern.MatchString(info.Name()):
			header.Mode = int64(pkgFile.ConfigMode & os.ModePerm)
		case info.Mode().IsRegular() && pkgFile.Mode > 0:
			header.Mode = int64(pkgFile.Mode & os.ModePerm)
		case info.IsDir():
			header.Mode = int64(0755)
		}

		if filepath.IsAbs(pkgFile.Target) {
			baseDir = ""
		}

		relPath, err := filepath.Rel(pkgFile.Source, path)
		if err != nil {
			return err
		}

		header.Name = filepath.Join(baseDir, pkgFile.Target, relPath)
		if info.IsDir() {
			header.Name += string(filepath.Separator)
		}

		if mg.Verbose() {
			log.Println("Adding", os.FileMode(mustConvertToUnit32(header.Mode)), header.Name)
		}
		if err := ar.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		if _, err = io.Copy(ar, file); err != nil {
			return err
		}
		return file.Close()
	})
}

// addSymlinkToTar adds a symlink file  to a tar archive.
func addSymlinkToTar(tmpdir string, ar *tar.Writer, baseDir string, pkgFile PackageFile) error {
	// create symlink we can work with later, header will be updated later
	link := filepath.Join(tmpdir, "link")
	target := tmpdir
	if err := os.Symlink(target, link); err != nil {
		return err
	}

	return filepath.Walk(link, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if pkgFile.SkipOnMissing && os.IsNotExist(err) {
				return nil
			}

			return err
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}
		header.Uname, header.Gname = "root", "root"
		header.Uid, header.Gid = 0, 0

		switch {
		case componentConfigFilePattern.MatchString(info.Name()):
			header.Mode = int64(componentConfigMode & os.ModePerm)
		case pkgFile.ConfigMode > 0 && configFilePattern.MatchString(info.Name()):
			header.Mode = int64(pkgFile.ConfigMode & os.ModePerm)
		case info.Mode().IsRegular() && pkgFile.Mode > 0:
			header.Mode = int64(pkgFile.Mode & os.ModePerm)
		case info.IsDir():
			header.Mode = int64(0755)
		}

		header.Name = filepath.Join(baseDir, pkgFile.Target)
		if filepath.IsAbs(pkgFile.Target) {
			header.Name = pkgFile.Target
		}

		header.Linkname = pkgFile.Source
		header.Typeflag = tar.TypeSymlink

		if mg.Verbose() {
			log.Println("Adding", os.FileMode(mustConvertToUnit32(header.Mode)), header.Name)
		}
		if err := ar.WriteHeader(header); err != nil {
			return err
		}

		return nil
	})
}

// PackageDocker packages the Beat into a docker image.
func PackageDocker(spec PackageSpec) error {
	if err := HaveDocker(); err != nil {
		return fmt.Errorf("docker daemon required to build images: %w", err)
	}

	b, err := newDockerBuilder(spec)
	if err != nil {
		return err
	}
	return b.Build()
}

func mustConvertToUnit32(i int64) uint32 {
	if i > math.MaxUint32 {
		panic(fmt.Sprintf("%d is bigger than math.MaxUint32", i))
	}
	return uint32(i) // #nosec
}
