// Package update contains the types and functions used by the update system.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/fatih/color"
	"github.com/fleetdm/fleet/v4/orbit/pkg/constant"
	"github.com/fleetdm/fleet/v4/orbit/pkg/platform"
	"github.com/fleetdm/fleet/v4/pkg/file"
	"github.com/fleetdm/fleet/v4/pkg/fleethttp"
	"github.com/fleetdm/fleet/v4/pkg/secure"
	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/rs/zerolog/log"
	"github.com/theupdateframework/go-tuf/client"
	"github.com/theupdateframework/go-tuf/data"
)

const (
	binDir     = "bin"
	stagingDir = "staging"

	defaultURL      = "https://tuf.fleetctl.com"
	defaultRootKeys = `[{"keytype":"ed25519","scheme":"ed25519","keyid_hash_algorithms":["sha256","sha512"],"keyval":{"public":"6d71d3beac3b830be929f2b10d513448d49ec6bb62a680176b89ffdfca180eb4"}}]`
)

// Updater is responsible for managing update state.
//
// Updater supports updating plain executables and
// .tar.gz compressed executables.
type Updater struct {
	opt    Options
	client *client.Client
}

// Options are the options that can be provided when creating an Updater.
type Options struct {
	// RootDirectory is the root directory from which other directories should be referenced.
	RootDirectory string
	// ServerURL is the URL of the update server.
	ServerURL string
	// InsecureTransport skips TLS certificate verification in the transport if
	// set to true. Best to leave this on, but due to the file signing any
	// tampering by a MitM should be detectable.
	InsecureTransport bool
	// RootKeys is the JSON encoded root keys to use to bootstrap trust.
	RootKeys string
	// LocalStore is the local metadata store.
	LocalStore client.LocalStore
	// Targets holds the targets the Updater keeps track of.
	Targets Targets
}

// Targets is a map of target name and its tracking information.
type Targets map[string]TargetInfo

// SetTargetChannel sets the channel of a target in the map.
func (ts Targets) SetTargetChannel(target, channel string) {
	t := ts[target]
	t.Channel = channel
	ts[target] = t
}

// TargetInfo holds all the information to track target updates.
type TargetInfo struct {
	// Platform is the target's platform string.
	Platform string
	// Channel is the target's update channel.
	Channel string
	// TargetFile is the name of the target file in the repository.
	TargetFile string
	// ExtractedExecSubPath is the path to the executable in case the
	// target is a compressed file.
	ExtractedExecSubPath []string
}

// New creates a new updater given the provided options. All the necessary
// directories are initialized.
func New(opt Options) (*Updater, error) {
	if opt.LocalStore == nil {
		return nil, errors.New("opt.LocalStore must be non-nil")
	}

	httpClient := fleethttp.NewClient(fleethttp.WithTLSClientConfig(&tls.Config{
		InsecureSkipVerify: opt.InsecureTransport,
	}))

	remoteStore, err := client.HTTPRemoteStore(opt.ServerURL, nil, httpClient)
	if err != nil {
		return nil, fmt.Errorf("init remote store: %w", err)
	}

	tufClient := client.NewClient(opt.LocalStore, remoteStore)
	var rootKeys []*data.PublicKey
	if err := json.Unmarshal([]byte(opt.RootKeys), &rootKeys); err != nil {
		return nil, fmt.Errorf("unmarshal root keys: %w", err)
	}

	meta, err := opt.LocalStore.GetMeta()
	if err != nil || meta["root.json"] == nil {
		var rootKeys []*data.PublicKey
		if err := json.Unmarshal([]byte(opt.RootKeys), &rootKeys); err != nil {
			return nil, fmt.Errorf("unmarshal root keys: %w", err)
		}
		if err := tufClient.Init(rootKeys, 1); err != nil {
			return nil, fmt.Errorf("init tuf client: %w", err)
		}
	}

	updater := &Updater{
		opt:    opt,
		client: tufClient,
	}

	if err := updater.initializeDirectories(); err != nil {
		return nil, err
	}

	return updater, nil
}

// NewDisabled creates a new disabled Updater. A disabled updater
// won't reach out for a remote repository.
//
// A disabled updater is useful to use local paths the way an
// enabled Updater would (to locate executables on environments
// where updates and/or network access are disabled).
func NewDisabled(opt Options) *Updater {
	return &Updater{
		opt: opt,
	}
}

// UpdateMetadata downloads and verifies remote repository metadata.
func (u *Updater) UpdateMetadata() error {
	if _, err := u.client.Update(); err != nil {
		// An error is returned if we are already up-to-date. We can ignore that
		// error.
		if !client.IsLatestSnapshot(ctxerr.Cause(err)) {
			return fmt.Errorf("update metadata: %w", err)
		}
	}
	return nil
}

// repoPath returns the path of the target in the remote repository.
func (u *Updater) repoPath(target string) (string, error) {
	t, ok := u.opt.Targets[target]
	if !ok {
		return "", fmt.Errorf("unknown target: %s", target)
	}
	return path.Join(target, t.Platform, t.Channel, t.TargetFile), nil
}

// ExecutableLocalPath returns the configured executable local path of a target.
func (u *Updater) ExecutableLocalPath(target string) (string, error) {
	localTarget, err := u.localTarget(target)
	if err != nil {
		return "", err
	}
	return localTarget.ExecPath, nil
}

// DirLocalPath returns the configured root directory local path of a tar.gz target.
//
// Returns empty for a non tar.gz target.
func (u *Updater) DirLocalPath(target string) (string, error) {
	localTarget, err := u.localTarget(target)
	if err != nil {
		return "", err
	}
	return localTarget.DirPath, nil
}

// LocalTarget holds local paths of a target.
//
// E.g., for a osqueryd target:
//
//	LocalTarget{
//		Info: TargetInfo{
//			Platform:             "macos-app",
//			Channel:              "stable",
//			TargetFile:           "osqueryd.app.tar.gz",
//			ExtractedExecSubPath: []string{"osquery.app", "Contents", "MacOS", "osqueryd"},
//		},
//		Path: "/local/path/to/osqueryd.app.tar.gz",
//		DirPath: "/local/path/to/osqueryd.app",
//		ExecPath: "/local/path/to/osqueryd.app/Contents/MacOS/osqueryd",
//	}
type LocalTarget struct {
	// Info holds the TUF target and package structure info.
	Info TargetInfo
	// Path holds the location of the target as downloaded from TUF.
	Path string
	// DirPath holds the path of the extracted target.
	//
	// DirPath is empty for non-tar.gz targets.
	DirPath string
	// ExecPath is the path of the executable.
	ExecPath string
}

// localTarget returns the info and local path of a target.
func (u *Updater) localTarget(target string) (*LocalTarget, error) {
	t, ok := u.opt.Targets[target]
	if !ok {
		return nil, fmt.Errorf("unknown target: %s", target)
	}
	lt := &LocalTarget{
		Info: t,
		Path: filepath.Join(
			u.opt.RootDirectory, binDir, t.TargetFile,
		),
	}
	lt.ExecPath = lt.Path
	if strings.HasSuffix(lt.Path, ".tar.gz") {
		lt.ExecPath = filepath.Join(append([]string{filepath.Dir(lt.Path)}, t.ExtractedExecSubPath...)...)
		lt.DirPath = filepath.Join(filepath.Dir(lt.Path), lt.Info.ExtractedExecSubPath[0])
	}
	return lt, nil
}

// Lookup looks up the provided target in the local target metadata. This should
// be called after UpdateMetadata.
func (u *Updater) Lookup(target string) (*data.TargetFileMeta, error) {
	repoPath, err := u.repoPath(target)
	if err != nil {
		return nil, err
	}
	t, err := u.client.Target(repoPath)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", target, err)
	}
	return &t, nil
}

// Targets gets all of the known targets
func (u *Updater) Targets() (data.TargetFiles, error) {
	targets, err := u.client.Targets()
	if err != nil {
		return nil, fmt.Errorf("get targets: %w", err)
	}

	return targets, nil
}

// Get downloads (if it doesn't exist) a target and returns its local information.
func (u *Updater) Get(target string) (*LocalTarget, error) {
	if target == "" {
		return nil, errors.New("target is required")
	}

	localTarget, err := u.localTarget(target)
	if err != nil {
		return nil, fmt.Errorf("failed to load local path for target %s: %w", target, err)
	}
	repoPath, err := u.repoPath(target)
	if err != nil {
		return nil, fmt.Errorf("failed to load repository path for target %s: %w", target, err)
	}

	switch stat, err := os.Stat(localTarget.Path); {
	case err == nil:
		if !stat.Mode().IsRegular() {
			return nil, fmt.Errorf("expected %s to be regular file", localTarget.Path)
		}
		meta, err := u.Lookup(target)
		if err != nil {
			return nil, err
		}
		if err := checkFileHash(meta, localTarget.Path); err != nil {
			log.Debug().Str("info", err.Error()).Msg("change detected")
			if err := u.download(target, repoPath, localTarget.Path); err != nil {
				return nil, fmt.Errorf("download %q: %w", repoPath, err)
			}
			if strings.HasSuffix(localTarget.Path, ".tar.gz") {
				if err := os.RemoveAll(localTarget.DirPath); err != nil {
					return nil, fmt.Errorf("failed to remove old extracted dir: %q: %w", localTarget.DirPath, err)
				}
			}
		} else {
			log.Debug().Str("path", localTarget.Path).Str("target", target).Msg("found expected target locally")
		}
	case errors.Is(err, os.ErrNotExist):
		log.Debug().Err(err).Msg("stat file")
		if err := u.download(target, repoPath, localTarget.Path); err != nil {
			return nil, fmt.Errorf("download %q: %w", repoPath, err)
		}
	default:
		return nil, fmt.Errorf("stat %q: %w", localTarget.Path, err)
	}

	if strings.HasSuffix(localTarget.Path, ".tar.gz") {
		s, err := os.Stat(localTarget.ExecPath)
		switch {
		case err == nil:
			// OK
		case errors.Is(err, os.ErrNotExist):
			if err := extractTarGz(localTarget.Path); err != nil {
				return nil, fmt.Errorf("extract %q: %w", localTarget.Path, err)
			}
			s, err = os.Stat(localTarget.ExecPath)
			if err != nil {
				return nil, fmt.Errorf("stat %q: %w", localTarget.ExecPath, err)
			}
		default:
			return nil, fmt.Errorf("stat %q: %w", localTarget.ExecPath, err)
		}
		if !s.Mode().IsRegular() {
			return nil, fmt.Errorf("expected a regular file: %q", localTarget.ExecPath)
		}
	}

	return localTarget, nil
}

func writeDevWarningBanner(w io.Writer) {
	warningColor := color.New(color.FgWhite, color.Bold, color.BgRed)
	warningColor.Fprintf(w, "WARNING: You are attempting to override orbit with a dev build.\nPress Enter to continue, or Control-c to exit.")
	// We need to disable color and print a new line to make it look somewhat neat, otherwise colors continue to the
	// next line
	warningColor.DisableColor()
	warningColor.Fprintln(w)
	bufio.NewScanner(os.Stdin).Scan()
}

// CopyDevBuilds uses a development build for the given target+channel.
//
// This is just for development, must not be used in production.
func (u *Updater) CopyDevBuild(target, devBuildPath string) {
	writeDevWarningBanner(os.Stderr)

	localPath, err := u.ExecutableLocalPath(target)
	if err != nil {
		panic(err)
	}
	if err := secure.MkdirAll(filepath.Dir(localPath), constant.DefaultDirMode); err != nil {
		panic(err)
	}
	dst, err := secure.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, constant.DefaultExecutableMode)
	if err != nil {
		panic(err)
	}
	defer dst.Close()

	src, err := secure.OpenFile(devBuildPath, os.O_RDONLY, constant.DefaultExecutableMode)
	if err != nil {
		panic(err)
	}
	defer src.Close()

	if _, err := src.Stat(); err != nil {
		panic(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		panic(err)
	}
}

// download downloads the target to the provided path. The file is deleted and
// an error is returned if the hash does not match.
func (u *Updater) download(target, repoPath, localPath string) error {
	staging := filepath.Join(u.opt.RootDirectory, stagingDir)

	if err := secure.MkdirAll(staging, constant.DefaultDirMode); err != nil {
		return fmt.Errorf("initialize download dir: %w", err)
	}

	// Additional chmod only necessary on Windows, effectively a no-op on other
	// platforms.
	if err := platform.ChmodExecutableDirectory(staging); err != nil {
		return err
	}

	tmp, err := secure.OpenFile(
		filepath.Join(staging, filepath.Base(localPath)),
		os.O_CREATE|os.O_WRONLY,
		constant.DefaultExecutableMode,
	)
	if err != nil {
		return fmt.Errorf("open temp file for download: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err := platform.ChmodExecutable(tmp.Name()); err != nil {
		return fmt.Errorf("chmod download: %w", err)
	}

	if err := secure.MkdirAll(filepath.Dir(localPath), constant.DefaultDirMode); err != nil {
		return fmt.Errorf("initialize download dir: %w", err)
	}

	// Additional chmod only necessary on Windows, effectively a no-op on other
	// platforms.
	if err := platform.ChmodExecutableDirectory(filepath.Dir(localPath)); err != nil {
		return err
	}

	// The go-tuf client handles checking of max size and hash.
	if err := u.client.Download(repoPath, &fileDestination{tmp}); err != nil {
		return fmt.Errorf("download target %s: %w", repoPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp file: %w", err)
	}

	if err := u.checkExec(target, tmp.Name()); err != nil {
		return fmt.Errorf("exec check failed %q: %w", tmp.Name(), err)
	}

	if runtime.GOOS == "windows" {
		// Remove old file first
		if err := os.Rename(localPath, localPath+".old"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rename old: %w", err)
		}
	}

	if err := os.Rename(tmp.Name(), localPath); err != nil {
		return fmt.Errorf("move download: %w", err)
	}

	return nil
}

func goosFromPlatform(platform string) (string, error) {
	switch platform {
	case "macos", "macos-app":
		return "darwin", nil
	case "windows", "linux":
		return platform, nil
	default:
		return "", fmt.Errorf("unknown platform: %s", platform)
	}
}

// checkExec checks/verifies a downloaded executable target by executing it.
func (u *Updater) checkExec(target, tmpPath string) error {
	localTarget, err := u.localTarget(target)
	if err != nil {
		return err
	}
	platformGOOS, err := goosFromPlatform(localTarget.Info.Platform)
	if err != nil {
		return err
	}
	if platformGOOS != runtime.GOOS {
		// Nothing to do, we can't check the executable if running cross-platform.
		// This generally happens when generating a package from a different platform
		// than the target package (e.g. generating an MSI package from macOS).
		return nil
	}

	if strings.HasSuffix(tmpPath, ".tar.gz") {
		if err := extractTarGz(tmpPath); err != nil {
			return fmt.Errorf("extract %q: %w", tmpPath, err)
		}
		tmpDirPath := filepath.Join(filepath.Dir(tmpPath), localTarget.Info.ExtractedExecSubPath[0])
		defer os.RemoveAll(tmpDirPath)
		tmpPath = filepath.Join(append([]string{filepath.Dir(tmpPath)}, localTarget.Info.ExtractedExecSubPath...)...)
	}

	// Note that this would fail for any binary that returns nonzero for --help.
	out, err := exec.Command(tmpPath, "--help").CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec new version: %s: %w", string(out), err)
	}
	return nil
}

// extractTagGz extracts the contents of the provided tar.gz file.
func extractTarGz(path string) error {
	tarGzFile, err := secure.OpenFile(path, os.O_RDONLY, 0o755)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer tarGzFile.Close()

	gzipReader, err := gzip.NewReader(tarGzFile)
	if err != nil {
		return fmt.Errorf("gzip reader %q: %w", path, err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		switch {
		case err == nil:
			// OK
		case errors.Is(err, io.EOF):
			return nil
		default:
			return fmt.Errorf("tar reader %q: %w", path, err)
		}

		// Prevent zip-slip attack.
		if strings.Contains(header.Name, "..") {
			return fmt.Errorf("invalid path in tar.gz: %q", header.Name)
		}

		targetPath := filepath.Join(filepath.Dir(path), header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := secure.MkdirAll(targetPath, constant.DefaultDirMode); err != nil {
				return fmt.Errorf("mkdir %q: %w", header.Name, err)
			}
		case tar.TypeReg:
			err := func() error {
				outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, header.FileInfo().Mode())
				if err != nil {
					return fmt.Errorf("failed to create %q: %w", header.Name, err)
				}
				defer outFile.Close()

				if _, err := io.Copy(outFile, tarReader); err != nil {
					return fmt.Errorf("failed to copy %q: %w", header.Name, err)
				}
				return nil
			}()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown flag type %q: %d", header.Name, header.Typeflag)
		}
	}
}

func (u *Updater) initializeDirectories() error {
	for _, dir := range []string{
		filepath.Join(u.opt.RootDirectory, binDir),
	} {
		err := secure.MkdirAll(dir, constant.DefaultDirMode)
		if err != nil {
			return fmt.Errorf("initialize directories: %w", err)
		}
	}

	return nil
}

type FileMigration struct {
	OldPath string
	NewPath string
}

// MigrateRoot mirates a previous installation to the new-style paths (linux only). If it returns true, then a migration
// occured and orbit should exit.
func MigrateRoot(opt Options) (bool, error) {

	// check if executable is located in root dir
	executable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("get executable path: %w", err)
	}

	// find the old root dir, which contains bin directory
	systemRoot := getSystemRoot()
	path := executable
	for path != systemRoot && filepath.Base(path) != "bin" {
		path = filepath.Dir(path)
	}
	if path == systemRoot {
		return false, errors.New("old orbit root dir not found")
	}
	oldRoot := filepath.Dir(path)

	if oldRoot == opt.RootDirectory {
		// root directory is the same. Either this is a fresh install, or has already migrated
		return false, nil
	}

	log.Info().Msg("migrating to new orbit root directory")

	// migrate binaries and config files

	// binaries
	var migrations []FileMigration
	for target, targetInfo := range opt.Targets {
		migrations = append(migrations, FileMigration{
			OldPath: filepath.Join(oldRoot, "bin", target, targetInfo.Platform, targetInfo.Channel, targetInfo.TargetFile),
			NewPath: filepath.Join(opt.RootDirectory, "bin", targetInfo.TargetFile),
		})
	}

	// config files
	migrations = append(migrations, []FileMigration{
		{
			OldPath: filepath.Join(oldRoot, "certs.pem"),
			NewPath: filepath.Join(opt.RootDirectory, "certs.pem"),
		},
		{
			OldPath: filepath.Join(oldRoot, "osquery.flags"),
			NewPath: filepath.Join(opt.RootDirectory, "osquery.flags"),
		},
		{
			OldPath: filepath.Join(oldRoot, "tuf-metadata.json"),
			NewPath: filepath.Join(opt.RootDirectory, "tuf-metadata.json"),
		},
		{
			OldPath: filepath.Join(oldRoot, "fleet.pem"),
			NewPath: filepath.Join(opt.RootDirectory, "fleet.pem"),
		},
	}...)
	switch runtime.GOOS {
	case "windows":
		migrations = append(migrations, FileMigration{
			OldPath: filepath.Join(oldRoot, "secret.txt"),
			NewPath: filepath.Join(opt.RootDirectory, "secret.txt"),
		})
	case "linux":
		migrations = append(migrations, FileMigration{
			OldPath: filepath.Join("/", "etc", "default", "orbit"),
			NewPath: filepath.Join(opt.RootDirectory, "env", "orbit"),
		})
	case "darwin":
		migrations = append(migrations, FileMigration{
			OldPath: filepath.Join(oldRoot, "secret.txt"),
			NewPath: filepath.Join(opt.RootDirectory, "secret.txt"),
		})
	}

	for _, migration := range migrations {
		log.Info().Msgf("moving %s to %s", migration.OldPath, migration.NewPath)
		err := os.MkdirAll(filepath.Dir(migration.NewPath), constant.DefaultDirMode)
		if err != nil {
			return false, err
		}

		// make sure all these files have the same permissions
		err = file.CopyWithPerms(migration.OldPath, migration.NewPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("move %s to %s: %w", migration.OldPath, migration.NewPath, err)
		}
	}

	orbitPath := filepath.Join(opt.RootDirectory, "bin", "orbit")

	switch runtime.GOOS {
	case "windows":
		// edit the existing windows service
		scPath, err := exec.LookPath("SC.exe")
		if err != nil {
			return false, fmt.Errorf("find systemctl in path: %w", err)
		}

		// get the current binPath, because it contains args that could have been modified since installation
		cmd := exec.Command(scPath, "qc", "Fleet osquery")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("get service config: %w", err)
		}

		scanner := bufio.NewScanner(bytes.NewBuffer(out))
		var binPath string
		for scanner.Scan() {
			f := strings.Fields(scanner.Text())
			if len(f) > 1 && f[0] == "BINARY_PATH_NAME" {
				args := f[2:]
				args[0] = orbitPath
				binPath = strings.Join(args, " ")
				break
			}
		}
		if binPath == "" {
			return false, fmt.Errorf("get binary path")
		}

		cmd = exec.Command(scPath, "config", "Fleet osquery", "binpath=", binPath)
		out, err = cmd.CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("edit service: %s: %w", string(out), err)
		}
	case "linux":
		// update paths in systemd service file
		servicePath := filepath.Join("/", "usr", "lib", "systemd", "system", "orbit.service")
		log.Debug().Msgf("updating paths in %s", servicePath)
		b, err := os.ReadFile(servicePath)
		if err != nil {
			return false, err
		}

		re, err := regexp.Compile(`(?m)^(EnvironmentFile=).*`)
		if err != nil {
			return false, err
		}
		environmentFilePath := filepath.Join(opt.RootDirectory, "env", "orbit")
		b = re.ReplaceAll(b, []byte("$1"+environmentFilePath))

		re, err = regexp.Compile(`(?m)^(ExecStart=).*`)
		if err != nil {
			return false, err
		}
		orbitPath := filepath.Join(opt.RootDirectory, "bin", "orbit")
		b = re.ReplaceAll(b, []byte("$1"+orbitPath))

		err = os.WriteFile(servicePath, b, 0)
		if err != nil {
			return false, fmt.Errorf("write %s: %w", servicePath, err)
		}

		// call daemon-reload so that it restarts the service with the updated orbit.service unit file
		systemctlPath, err := exec.LookPath("systemctl")
		if err != nil && err != exec.ErrNotFound {
			return false, fmt.Errorf("find systemctl in path: %w", err)
		} else if err == nil {
			log.Debug().Msg("reloading unit files ...")
			cmd := exec.Command(systemctlPath, "daemon-reload")
			out, err := cmd.CombinedOutput()
			if err != nil {
				// this is a problem since the service will not be restarted unless reload is successful
				log.Error().Err(err).Msgf("systemctl daemon-reload returned an error: %s", string(out))
				return false, fmt.Errorf("systemctl daemon-reload: %w", err)
			}
		}
	case "darwin":
		plistPath := filepath.Join("/", "Library", "LaunchDaemons", "com.fleetdm.orbit.plist")
		log.Debug().Msgf("updating paths in %s", plistPath)

		// update orbit path using defaults command
		cmd := exec.Command("defaults", "write", plistPath, "ProgramArguments", "-array", "-string", orbitPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("defaults read %s: %s: %w", plistPath, out, err)
		}

		// force reload the service
		// cmd = exec.Command("defaults", "read", plistPath)
		// out err := cmd.CombinedOutput()
		// if err != nil {
		// 	return false, fmt.Errorf("defaults read %s: %s: %w", plistPath, out, err)
		// }
	}

	// clean up old files
	removePaths := []string{
		oldRoot,
		filepath.Join("etc", "default", "orbit"),
		filepath.Join("usr", "local", "bin", "orbit"),
	}
	for _, path := range removePaths {
		log.Debug().Msgf("removing %s", path)
		if err := os.RemoveAll(path); err != nil {
			// An error here is okay, orbit will be fine at this point
			log.Error().Err(err).Msgf("failed to remove %s", path)
		}
	}

	return true, nil
}

// getSystemRoot gets the system getSystemRoot directory, works on windows and unix
func getSystemRoot() string {
	return os.Getenv("SystemDrive") + string(os.PathSeparator)
}
