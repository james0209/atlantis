// Copyright 2017 HootSuite Media Inc.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an AS IS BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// Modified hereafter by contributors to runatlantis/atlantis.
//
// Package terraform handles the actual running of terraform commands.
package terraform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-getter/v2"
	"github.com/hashicorp/go-version"
	install "github.com/hashicorp/hc-install"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hc-install/src"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"

	"github.com/runatlantis/atlantis/server/core/runtime/models"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/terraform/ansi"
	"github.com/runatlantis/atlantis/server/jobs"
	"github.com/runatlantis/atlantis/server/logging"
)

var LogStreamingValidCmds = [...]string{"init", "plan", "apply"}

//go:generate pegomock generate --package mocks -o mocks/mock_terraform_client.go Client

type Client interface {
	// RunCommandWithVersion executes terraform with args in path. If v is nil,
	// it will use the default Terraform version. workspace is the Terraform
	// workspace which should be set as an environment variable.
	RunCommandWithVersion(ctx command.ProjectContext, path string, args []string, envs map[string]string, v *version.Version, workspace string) (string, error)

	// EnsureVersion makes sure that terraform version `v` is available to use
	EnsureVersion(log logging.SimpleLogging, v *version.Version) error

	// DetectVersion Extracts required_version from Terraform configuration in the specified project directory. Returns nil if unable to determine the version.
	DetectVersion(log logging.SimpleLogging, projectDirectory string) *version.Version
}

type DefaultClient struct {
	// defaultVersion is the default version of terraform to use if another
	// version isn't specified.
	defaultVersion *version.Version
	// We will run terraform with the TF_PLUGIN_CACHE_DIR env var set to this
	// directory inside our data dir.
	terraformPluginCacheDir string
	binDir                  string
	// overrideTF can be used to override the terraform binary during testing
	// with another binary, ex. echo.
	overrideTF string
	// downloader downloads terraform versions.
	downloader      Downloader
	downloadBaseURL string
	downloadAllowed bool
	// versions maps from the string representation of a tf version (ex. 0.11.10)
	// to the absolute path of that binary on disk (if it exists).
	// Use versionsLock to control access.
	versions map[string]string

	// versionsLock is used to ensure versions isn't being concurrently written to.
	versionsLock *sync.Mutex

	// usePluginCache determines whether or not to set the TF_PLUGIN_CACHE_DIR env var
	usePluginCache bool

	projectCmdOutputHandler jobs.ProjectCommandOutputHandler
}

//go:generate pegomock generate --package mocks -o mocks/mock_downloader.go Downloader

// Downloader is for downloading terraform versions.
type Downloader interface {
	Install(dir string, v *version.Version) (string, error)
	GetFile(dst, src string) error
	GetAny(dst, src string) error
}

// versionRegex extracts the version from `terraform version` output.
//
//	    Terraform v0.12.0-alpha4 (2c36829d3265661d8edbd5014de8090ea7e2a076)
//		   => 0.12.0-alpha4
//
//	    Terraform v0.11.10
//		   => 0.11.10
var versionRegex = regexp.MustCompile("Terraform v(.*?)(\\s.*)?\n")

// NewClientWithDefaultVersion creates a new terraform client and pre-fetches the default version
func NewClientWithDefaultVersion(
	log logging.SimpleLogging,
	binDir string,
	cacheDir string,
	tfeToken string,
	tfeHostname string,
	defaultVersionStr string,
	defaultVersionFlagName string,
	tfDownloadURL string,
	tfDownloader Downloader,
	tfDownloadAllowed bool,
	usePluginCache bool,
	fetchAsync bool,
	projectCmdOutputHandler jobs.ProjectCommandOutputHandler,
) (*DefaultClient, error) {
	var finalDefaultVersion *version.Version
	var localVersion *version.Version
	versions := make(map[string]string)
	var versionsLock sync.Mutex

	localPath, err := exec.LookPath("terraform")
	if err != nil && defaultVersionStr == "" {
		return nil, fmt.Errorf("terraform not found in $PATH. Set --%s or download terraform from https://developer.hashicorp.com/terraform/downloads", defaultVersionFlagName)
	}
	if err == nil {
		localVersion, err = getVersion(localPath)
		if err != nil {
			return nil, err
		}
		versions[localVersion.String()] = localPath
		if defaultVersionStr == "" {
			// If they haven't set a default version, then whatever they had
			// locally is now the default.
			finalDefaultVersion = localVersion
		}
	}

	if defaultVersionStr != "" {
		defaultVersion, err := version.NewVersion(defaultVersionStr)
		if err != nil {
			return nil, err
		}
		finalDefaultVersion = defaultVersion
		ensureVersionFunc := func() {
			// Since ensureVersion might end up downloading terraform,
			// we call it asynchronously so as to not delay server startup.
			versionsLock.Lock()
			_, err := ensureVersion(log, tfDownloader, versions, defaultVersion, binDir, tfDownloadURL, tfDownloadAllowed)
			versionsLock.Unlock()
			if err != nil {
				log.Err("could not download terraform %s: %s", defaultVersion.String(), err)
			}
		}

		if fetchAsync {
			go ensureVersionFunc()
		} else {
			ensureVersionFunc()
		}
	}

	// If tfeToken is set, we try to create a ~/.terraformrc file.
	if tfeToken != "" {
		home, err := homedir.Dir()
		if err != nil {
			return nil, errors.Wrap(err, "getting home dir to write ~/.terraformrc file")
		}
		if err := generateRCFile(tfeToken, tfeHostname, home); err != nil {
			return nil, err
		}
	}
	return &DefaultClient{
		defaultVersion:          finalDefaultVersion,
		terraformPluginCacheDir: cacheDir,
		binDir:                  binDir,
		downloader:              tfDownloader,
		downloadBaseURL:         tfDownloadURL,
		downloadAllowed:         tfDownloadAllowed,
		versionsLock:            &versionsLock,
		versions:                versions,
		usePluginCache:          usePluginCache,
		projectCmdOutputHandler: projectCmdOutputHandler,
	}, nil

}

func NewTestClient(
	log logging.SimpleLogging,
	binDir string,
	cacheDir string,
	tfeToken string,
	tfeHostname string,
	defaultVersionStr string,
	defaultVersionFlagName string,
	tfDownloadURL string,
	tfDownloader Downloader,
	tfDownloadAllowed bool,
	usePluginCache bool,
	projectCmdOutputHandler jobs.ProjectCommandOutputHandler,
) (*DefaultClient, error) {
	return NewClientWithDefaultVersion(
		log,
		binDir,
		cacheDir,
		tfeToken,
		tfeHostname,
		defaultVersionStr,
		defaultVersionFlagName,
		tfDownloadURL,
		tfDownloader,
		tfDownloadAllowed,
		usePluginCache,
		false,
		projectCmdOutputHandler,
	)
}

// NewClient constructs a terraform client.
// tfeToken is an optional terraform enterprise token.
// defaultVersionStr is an optional default terraform version to use unless
// a specific version is set.
// defaultVersionFlagName is the name of the flag that sets the default terraform
// version.
// tfDownloader is used to download terraform versions.
// Will asynchronously download the required version if it doesn't exist already.
func NewClient(
	log logging.SimpleLogging,
	binDir string,
	cacheDir string,
	tfeToken string,
	tfeHostname string,
	defaultVersionStr string,
	defaultVersionFlagName string,
	tfDownloadURL string,
	tfDownloader Downloader,
	tfDownloadAllowed bool,
	usePluginCache bool,
	projectCmdOutputHandler jobs.ProjectCommandOutputHandler,
) (*DefaultClient, error) {
	return NewClientWithDefaultVersion(
		log,
		binDir,
		cacheDir,
		tfeToken,
		tfeHostname,
		defaultVersionStr,
		defaultVersionFlagName,
		tfDownloadURL,
		tfDownloader,
		tfDownloadAllowed,
		usePluginCache,
		true,
		projectCmdOutputHandler,
	)
}

// Version returns the default version of Terraform we use if no other version
// is defined.
func (c *DefaultClient) DefaultVersion() *version.Version {
	return c.defaultVersion
}

// TerraformBinDir returns the directory where we download Terraform binaries.
func (c *DefaultClient) TerraformBinDir() string {
	return c.binDir
}

// ExtractExactRegex attempts to extract an exact version number from the provided string as a fallback.
// The function expects the version string to be in one of the following formats: "= x.y.z", "=x.y.z", or "x.y.z" where x, y, and z are integers.
// If the version string matches one of these formats, the function returns a slice containing the exact version number.
// If the version string does not match any of these formats, the function logs a debug message and returns nil.
func (c *DefaultClient) ExtractExactRegex(log logging.SimpleLogging, version string) []string {
	re := regexp.MustCompile(`^=?\s*([0-9.]+)\s*$`)
	matched := re.FindStringSubmatch(version)
	if len(matched) == 0 {
		log.Debug("exact version regex not found in the version %q", version)
		return nil
	}
	// The first element of the slice is the entire string, so we want the second element (the first capture group)
	tfVersions := []string{matched[1]}
	return tfVersions
}

// DetectVersion extracts required_version from Terraform configuration in the specified project directory. Returns nil if unable to determine the version.
// It will also try to evaluate non-exact matches by passing the Constraints to the hc-install Releases API, which will return a list of available versions.
// It will then select the highest version that satisfies the constraint.
func (c *DefaultClient) DetectVersion(log logging.SimpleLogging, projectDirectory string) *version.Version {
	module, diags := tfconfig.LoadModule(projectDirectory)
	if diags.HasErrors() {
		log.Err("trying to detect required version: %s", diags.Error())
	}

	if len(module.RequiredCore) != 1 {
		log.Info("cannot determine which version to use from terraform configuration, detected %d possibilities.", len(module.RequiredCore))
		return nil
	}
	requiredVersionSetting := module.RequiredCore[0]
	log.Debug("Found required_version setting of %q", requiredVersionSetting)

	if !c.downloadAllowed {
		log.Debug("terraform downloads disabled.")
		matched := c.ExtractExactRegex(log, requiredVersionSetting)
		if len(matched) == 0 {
			log.Debug("did not specify exact version in terraform configuration, found %q", requiredVersionSetting)
			return nil
		}

		version, err := version.NewVersion(matched[0])
		if err != nil {
			log.Err("error parsing version string: %s", err)
			return nil
		}
		return version
	}

	// Since terraform version 1.8.2, terraform is not a single file download anymore and
	// Atlantis fails to download version 1.8.2 and higher. So, as a short-term fix,
	// we need to block any version higher than 1.8.1 until proper solution is implemented.
	// More details on the issue here - https://github.com/runatlantis/atlantis/issues/4471
	constraintStr := fmt.Sprintf("%s, <= 1.8.1", requiredVersionSetting)
	vc, err := version.NewConstraint(constraintStr)
	if err != nil {
		log.Err("Error parsing constraint string: %s", err)
		return nil
	}

	constrainedVersions := &releases.Versions{
		Product:     product.Terraform,
		Constraints: vc,
	}
	installCandidates, err := constrainedVersions.List(context.Background())
	if err != nil {
		log.Err("error listing available versions: %s", err)
		return nil
	}
	if len(installCandidates) == 0 {
		log.Err("no Terraform versions found for constraints %s", constraintStr)
		return nil
	}

	// We want to select the highest version that satisfies the constraint.
	versionDownloader := installCandidates[len(installCandidates)-1]

	// Get the Version object from the versionDownloader.
	downloadVersion := versionDownloader.(*releases.ExactVersion).Version

	return downloadVersion
}

// See Client.EnsureVersion.
func (c *DefaultClient) EnsureVersion(log logging.SimpleLogging, v *version.Version) error {
	if v == nil {
		v = c.defaultVersion
	}

	var err error
	c.versionsLock.Lock()
	_, err = ensureVersion(log, c.downloader, c.versions, v, c.binDir, c.downloadBaseURL, c.downloadAllowed)
	c.versionsLock.Unlock()
	if err != nil {
		return err
	}

	return nil
}

// See Client.RunCommandWithVersion.
func (c *DefaultClient) RunCommandWithVersion(ctx command.ProjectContext, path string, args []string, customEnvVars map[string]string, v *version.Version, workspace string) (string, error) {
	if isAsyncEligibleCommand(args[0]) {
		_, outCh := c.RunCommandAsync(ctx, path, args, customEnvVars, v, workspace)

		var lines []string
		var err error
		for line := range outCh {
			if line.Err != nil {
				err = line.Err
				break
			}
			lines = append(lines, line.Line)
		}
		output := strings.Join(lines, "\n")

		// sanitize output by stripping out any ansi characters.
		output = ansi.Strip(output)
		return fmt.Sprintf("%s\n", output), err
	}
	tfCmd, cmd, err := c.prepExecCmd(ctx.Log, v, workspace, path, args)
	if err != nil {
		return "", err
	}
	envVars := cmd.Env
	for key, val := range customEnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, val))
	}
	cmd.Env = envVars
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)
	log := ctx.Log.With("duration", dur)
	if err != nil {
		err = errors.Wrapf(err, "running %q in %q", tfCmd, path)
		log.Err(err.Error())
		return ansi.Strip(string(out)), err
	}
	log.Info("successfully ran %q in %q", tfCmd, path)

	return ansi.Strip(string(out)), nil
}

// prepExecCmd builds a ready to execute command based on the version of terraform
// v, and args. It returns a printable representation of the command that will
// be run and the actual command.
func (c *DefaultClient) prepExecCmd(log logging.SimpleLogging, v *version.Version, workspace string, path string, args []string) (string, *exec.Cmd, error) {
	tfCmd, envVars, err := c.prepCmd(log, v, workspace, path, args)
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command("sh", "-c", tfCmd)
	cmd.Dir = path
	cmd.Env = envVars
	return tfCmd, cmd, nil
}

// prepCmd prepares a shell command (to be interpreted with `sh -c <cmd>`) and set of environment
// variables for running terraform.
func (c *DefaultClient) prepCmd(log logging.SimpleLogging, v *version.Version, workspace string, path string, args []string) (string, []string, error) {
	if v == nil {
		v = c.defaultVersion
	}

	var binPath string
	if c.overrideTF != "" {
		// This is only set during testing.
		binPath = c.overrideTF
	} else {
		var err error
		c.versionsLock.Lock()
		binPath, err = ensureVersion(log, c.downloader, c.versions, v, c.binDir, c.downloadBaseURL, c.downloadAllowed)
		c.versionsLock.Unlock()
		if err != nil {
			return "", nil, err
		}
	}

	// We add custom variables so that if `extra_args` is specified with env
	// vars then they'll be substituted.
	envVars := []string{
		// Will de-emphasize specific commands to run in output.
		"TF_IN_AUTOMATION=true",
		// Cache plugins so terraform init runs faster.
		fmt.Sprintf("WORKSPACE=%s", workspace),
		fmt.Sprintf("ATLANTIS_TERRAFORM_VERSION=%s", v.String()),
		fmt.Sprintf("DIR=%s", path),
	}
	if c.usePluginCache {
		envVars = append(envVars, fmt.Sprintf("TF_PLUGIN_CACHE_DIR=%s", c.terraformPluginCacheDir))
	}
	// Append current Atlantis process's environment variables, ex.
	// AWS_ACCESS_KEY.
	envVars = append(envVars, os.Environ()...)
	tfCmd := fmt.Sprintf("%s %s", binPath, strings.Join(args, " "))
	return tfCmd, envVars, nil
}

// RunCommandAsync runs terraform with args. It immediately returns an
// input and output channel. Callers can use the output channel to
// get the realtime output from the command.
// Callers can use the input channel to pass stdin input to the command.
// If any error is passed on the out channel, there will be no
// further output (so callers are free to exit).
func (c *DefaultClient) RunCommandAsync(ctx command.ProjectContext, path string, args []string, customEnvVars map[string]string, v *version.Version, workspace string) (chan<- string, <-chan models.Line) {
	cmd, envVars, err := c.prepCmd(ctx.Log, v, workspace, path, args)
	if err != nil {
		// The signature of `RunCommandAsync` doesn't provide for returning an immediate error, only one
		// once reading the output. Since we won't be spawning a process, simulate that by sending the
		// errorcustomEnvVars to the output channel.
		outCh := make(chan models.Line)
		inCh := make(chan string)
		go func() {
			outCh <- models.Line{Err: err}
			close(outCh)
			close(inCh)
		}()
		return inCh, outCh
	}

	for key, val := range customEnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, val))
	}

	runner := models.NewShellCommandRunner(cmd, envVars, path, true, c.projectCmdOutputHandler)
	inCh, outCh := runner.RunCommandAsync(ctx)
	return inCh, outCh
}

// MustConstraint will parse one or more constraints from the given
// constraint string. The string must be a comma-separated list of
// constraints. It panics if there is an error.
func MustConstraint(v string) version.Constraints {
	c, err := version.NewConstraint(v)
	if err != nil {
		panic(err)
	}
	return c
}

// ensureVersion returns the path to a terraform binary of version v.
// It will download this version if we don't have it.
func ensureVersion(
	log logging.SimpleLogging,
	dl Downloader,
	versions map[string]string,
	v *version.Version,
	binDir string,
	downloadURL string,
	downloadsAllowed bool,
) (string, error) {
	if binPath, ok := versions[v.String()]; ok {
		return binPath, nil
	}

	// This tf version might not yet be in the versions map even though it
	// exists on disk. This would happen if users have manually added
	// terraform{version} binaries. In this case we don't want to re-download.
	binFile := "terraform" + v.String()
	if binPath, err := exec.LookPath(binFile); err == nil {
		versions[v.String()] = binPath
		return binPath, nil
	}

	// The version might also not be in the versions map if it's in our bin dir.
	// This could happen if Atlantis was restarted without losing its disk.
	dest := filepath.Join(binDir, binFile)
	if _, err := os.Stat(dest); err == nil {
		versions[v.String()] = dest
		return dest, nil
	}
	if !downloadsAllowed {
		return "", fmt.Errorf(
			"could not find terraform version %s in PATH or %s, and downloads are disabled",
			v.String(),
			binDir,
		)
	}

	log.Info("could not find terraform version %s in PATH or %s", v.String(), binDir)

	if downloadURL != "https://releases.hashicorp.com" {
		log.Info("using a custom download URL %s to download Terraform version %s", downloadURL, v.String())
		urlPrefix := fmt.Sprintf("%s/terraform/%s/terraform_%s", downloadURL, v.String(), v.String())
		binURL := fmt.Sprintf("%s_%s_%s.zip", urlPrefix, runtime.GOOS, runtime.GOARCH)
		checksumURL := fmt.Sprintf("%s_SHA256SUMS", urlPrefix)
		fullSrcURL := fmt.Sprintf("%s?checksum=file:%s", binURL, checksumURL)
		if err := dl.GetFile(dest, fullSrcURL); err != nil {
			return "", errors.Wrapf(err, "downloading terraform version %s at %q", v.String(), fullSrcURL)
		}
	} else {
		log.Info("using Hashicorp's 'hc-install' to download Terraform version %s", v.String())
		_, err := dl.Install(binDir, v)
		if err != nil {
			return "", errors.Wrapf(err, "downloading terraform version %s", v.String())
		}
	}

	log.Info("Downloaded terraform %s to %s", v.String(), dest)
	versions[v.String()] = dest
	return dest, nil
}

// generateRCFile generates a .terraformrc file containing config for tfeToken
// and hostname tfeHostname.
// It will create the file in home/.terraformrc.
func generateRCFile(tfeToken string, tfeHostname string, home string) error {
	const rcFilename = ".terraformrc"
	rcFile := filepath.Join(home, rcFilename)
	config := fmt.Sprintf(rcFileContents, tfeHostname, tfeToken)

	// If there is already a .terraformrc file and its contents aren't exactly
	// what we would have written to it, then we error out because we don't
	// want to overwrite anything.
	if _, err := os.Stat(rcFile); err == nil {
		currContents, err := os.ReadFile(rcFile) // nolint: gosec
		if err != nil {
			return errors.Wrapf(err, "trying to read %s to ensure we're not overwriting it", rcFile)
		}
		if config != string(currContents) {
			return fmt.Errorf("can't write TFE token to %s because that file has contents that would be overwritten", rcFile)
		}
		// Otherwise we don't need to write the file because it already has
		// what we need.
		return nil
	}

	if err := os.WriteFile(rcFile, []byte(config), 0600); err != nil {
		return errors.Wrapf(err, "writing generated %s file with TFE token to %s", rcFilename, rcFile)
	}
	return nil
}

func isAsyncEligibleCommand(cmd string) bool {
	for _, validCmd := range LogStreamingValidCmds {
		if validCmd == cmd {
			return true
		}
	}
	return false
}

func getVersion(tfBinary string) (*version.Version, error) {
	versionOutBytes, err := exec.Command(tfBinary, "version").Output() // #nosec
	versionOutput := string(versionOutBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "running terraform version: %s", versionOutput)
	}
	match := versionRegex.FindStringSubmatch(versionOutput)
	if len(match) <= 1 {
		return nil, fmt.Errorf("could not parse terraform version from %s", versionOutput)
	}
	return version.NewVersion(match[1])
}

// rcFileContents is a format string to be used with Sprintf that can be used
// to generate the contents of a ~/.terraformrc file for authenticating with
// Terraform Enterprise.
var rcFileContents = `credentials "%s" {
  token = %q
}`

type DefaultDownloader struct{}

func (d *DefaultDownloader) Install(dir string, v *version.Version) (string, error) {
	installer := install.NewInstaller()
	execPath, err := installer.Install(context.Background(), []src.Installable{
		&releases.ExactVersion{
			Product:    product.Terraform,
			Version:    v,
			InstallDir: dir,
		},
	})
	if err != nil {
		return "", err
	}

	// hc-install installs terraform binary as just "terraform".
	// We need to rename it to terraform{version} to be consistent with current naming convention.
	newPath := filepath.Join(dir, "terraform"+v.String())
	if err := os.Rename(execPath, newPath); err != nil {
		return "", err
	}

	return newPath, nil
}

// See go-getter.GetFile.
func (d *DefaultDownloader) GetFile(dst, src string) error {
	_, err := getter.GetFile(context.Background(), dst, src)
	return err
}

// See go-getter.GetFile.
func (d *DefaultDownloader) GetAny(dst, src string) error {
	_, err := getter.GetAny(context.Background(), dst, src)
	return err
}
