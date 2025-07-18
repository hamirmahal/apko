// Copyright 2023 Chainguard, Inc.
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

package apk

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // this is what apk tools is using
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"go.lsp.dev/uri"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.step.sm/crypto/jose"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"gopkg.in/ini.v1"

	"chainguard.dev/apko/internal/tarfs"
	"chainguard.dev/apko/pkg/apk/auth"
	"chainguard.dev/apko/pkg/apk/expandapk"
	apkfs "chainguard.dev/apko/pkg/apk/fs"
	"chainguard.dev/apko/pkg/paths"

	"github.com/chainguard-dev/clog"
)

// This is terrible but simpler than plumbing around a cache for now.
// We just hold the expanded APK in memory rather than re-parsing it every time,
// which is expensive. This also dedupes simultaneous fetches.
var globalApkCache = &apkCache{}

type APK struct {
	arch               string
	version            string
	fs                 apkfs.FullFS
	executor           Executor
	ignoreMknodErrors  bool
	client             *http.Client
	cache              *cache
	ignoreSignatures   bool
	noSignatureIndexes []string
	auth               auth.Authenticator

	// filename to owning package, last write wins
	installedFiles map[string]*Package

	// This is a map of arch to apk.APK for every arch in a mult-arch situation.
	// It's stuffed here to avoid plumbing it across every method, but it's optional.
	ByArch map[string]*APK
}

func New(ctx context.Context, options ...Option) (*APK, error) {
	opt := defaultOpts()
	for _, o := range options {
		if err := o(opt); err != nil {
			return nil, err
		}
	}

	if opt.fs == nil {
		// This is expensive so we only want to do it if we aren't passed WithFS.
		opt.fs = apkfs.DirFS(ctx, "/")
	}

	client := retryablehttp.NewClient()

	client.HTTPClient = &http.Client{Transport: opt.transport}
	client.Logger = clog.FromContext(ctx)

	return &APK{
		client:             client.StandardClient(),
		fs:                 opt.fs,
		arch:               opt.arch,
		executor:           opt.executor,
		ignoreMknodErrors:  opt.ignoreMknodErrors,
		version:            opt.version,
		cache:              opt.cache,
		ignoreSignatures:   opt.ignoreSignatures,
		noSignatureIndexes: opt.noSignatureIndexes,
		installedFiles:     map[string]*Package{},
		auth:               opt.auth,
	}, nil
}

type directory struct {
	path  string
	perms os.FileMode
}
type file struct {
	path     string
	perms    os.FileMode
	contents []byte
}

type deviceFile struct {
	path  string
	major uint32
	minor uint32
	perms os.FileMode
}

var baseDirectories = []directory{
	{"/tmp", 0o777 | fs.ModeSticky},
	{"/dev", 0o755},
	{"/etc", 0o755},
	{"/opt", 0o755},
	{"/proc", 0o555},
	{"/var", 0o755},
	{"/usr", 0o755},
}

// directories is a list of directories to create relative to the root. It will not do MkdirAll, so you
// must include the parent.
// It assumes that the following directories already exist:
//
//		/var
//		/tmp
//		/dev
//		/etc
//	    /proc
var initDirectories = []directory{
	{"/etc/apk", 0o755},
	{"/etc/apk/keys", 0o755},
	{"/usr/lib", 0o755},
	{"/usr/lib/apk", 0o755},
	{"/usr/lib/apk/db", 0o755},
	{"/usr/lib/apk/exec", 0o755},
	{"/var/cache", 0o755},
	{"/var/cache/apk", 0o755},
	{"/var/cache/misc", 0o755},
}

// files is a list of files to create relative to the root, as well as optional content.
// We will not do MkdirAll for the parent dir it is in, so it must exist.
var initFiles = []file{
	{"/etc/apk/world", 0o644, []byte("\n")},
	{"/etc/apk/repositories", 0o644, []byte("\n")},
	{"/usr/lib/apk/db/lock", 0o600, nil},
	{"/usr/lib/apk/db/triggers", 0o644, nil},
	{"/usr/lib/apk/db/installed", 0o644, nil},
}

// deviceFiles is a list of files to create relative to the root.
var initDeviceFiles = []deviceFile{
	{"/dev/zero", 1, 5, 0o666},
	{"/dev/urandom", 1, 9, 0o666},
	{"/dev/null", 1, 3, 0o666},
	{"/dev/random", 1, 8, 0o666},
	{"/dev/console", 5, 1, 0o620},
}

// SetClient set the http client to use for downloading packages.
// In general, you can leave this unset, and it will use the default http.Client.
// It is useful for fine-grained control, for proxying, or for setting alternate
// paths.
func (a *APK) SetClient(client *http.Client) {
	a.client = client
}

// ListInitFiles list the files that are installed during the InitDB phase.
func (a *APK) ListInitFiles() []tar.Header {
	headers := make([]tar.Header, 0, 20)

	// additionalFiles are files we need but can only be resolved in the context of
	// this func, e.g. we need the architecture
	additionalFiles := []file{
		{"/etc/apk/arch", 0o644, []byte(a.arch + "\n")},
	}

	for _, e := range initDirectories {
		headers = append(headers, tar.Header{
			Name:     e.path,
			Mode:     int64(e.perms),
			Typeflag: tar.TypeDir,
			Uid:      0,
			Gid:      0,
		})
	}
	for _, e := range append(initFiles, additionalFiles...) {
		headers = append(headers, tar.Header{
			Name:     e.path,
			Mode:     int64(e.perms),
			Typeflag: tar.TypeReg,
			Uid:      0,
			Gid:      0,
		})
	}
	for _, e := range initDeviceFiles {
		headers = append(headers, tar.Header{
			Name:     e.path,
			Typeflag: tar.TypeChar,
			Mode:     int64(e.perms),
			Uid:      0,
			Gid:      0,
		})
	}

	// add scripts.tar with nothing in it
	headers = append(headers, tar.Header{
		Name:     scriptsFilePath,
		Mode:     int64(scriptsTarPerms),
		Typeflag: tar.TypeReg,
		Uid:      0,
		Gid:      0,
	})
	return headers
}

// Initialize the APK database for a given build context.
// Assumes base directories are in place and checks them.
// Returns the list of files and directories and files installed and permissions,
// unless those files will be included in the installed database, in which case they can
// be retrieved via GetInstalled().
func (a *APK) InitDB(ctx context.Context, buildRepos ...string) error {
	log := clog.FromContext(ctx)
	/*
		equivalent of: "apk add --initdb --arch arch --root root"
	*/
	log.Debug("initializing apk database")

	ctx, span := otel.Tracer("go-apk").Start(ctx, "InitDB")
	defer span.End()

	// additionalFiles are files we need but can only be resolved in the context of
	// this func, e.g. we need the architecture
	additionalFiles := []file{
		{"/etc/apk/arch", 0o644, []byte(a.arch + "\n")},
	}

	for _, e := range baseDirectories {
		stat, err := a.fs.Stat(e.path)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist):
			err := a.fs.Mkdir(e.path, e.perms)
			if err != nil {
				return fmt.Errorf("failed to create base directory %s: %w", e.path, err)
			}
		case err != nil:
			return fmt.Errorf("error opening base directory %s: %w", e.path, err)
		case !stat.IsDir():
			return fmt.Errorf("base directory %s is not a directory", e.path)
		case stat.Mode().Perm() != e.perms:
			return fmt.Errorf("base directory %s has incorrect permissions: %o", e.path, stat.Mode().Perm())
		}
	}
	for _, e := range initDirectories {
		err := a.fs.Mkdir(e.path, e.perms)
		switch {
		case err != nil && !errors.Is(err, fs.ErrExist):
			return fmt.Errorf("failed to create directory %s: %w", e.path, err)
		case err != nil && errors.Is(err, fs.ErrExist):
			stat, err := a.fs.Stat(e.path)
			if err != nil {
				return fmt.Errorf("failed to stat directory %s: %w", e.path, err)
			}
			if !stat.IsDir() {
				return fmt.Errorf("failed to create directory %s: already exists as file", e.path)
			}
		}
	}
	for _, e := range append(initFiles, additionalFiles...) {
		if err := a.fs.WriteFile(e.path, e.contents, e.perms); err != nil {
			return fmt.Errorf("failed to create file %s: %w", e.path, err)
		}
	}
	for _, e := range initDeviceFiles {
		perms := uint32(e.perms.Perm())
		err := a.fs.Mknod(e.path, unix.S_IFCHR|perms, int(unix.Mkdev(e.major, e.minor)))
		if !a.ignoreMknodErrors && err != nil {
			return fmt.Errorf("failed to create char device %s: %w", e.path, err)
		}
	}

	// add scripts.tar with nothing in it
	scriptsTarPerms := 0o644
	TarFile, err := a.fs.OpenFile(scriptsFilePath, os.O_CREATE|os.O_WRONLY, fs.FileMode(scriptsTarPerms))
	if err != nil {
		return fmt.Errorf("could not create tarball file '%s', got error '%w'", scriptsFilePath, err)
	}
	defer TarFile.Close()
	tarWriter := tar.NewWriter(TarFile)
	defer tarWriter.Close()

	// nothing to add to it; scripts.tar should be empty

	// Perform key discovery for the various build-time repositories.
	for _, repo := range buildRepos {
		if ver, ok := parseAlpineVersion(repo); ok {
			if err := a.fetchAlpineKeys(ctx, ver); err != nil {
				var nokeysErr *NoKeysFoundError
				if !errors.As(err, &nokeysErr) {
					return fmt.Errorf("failed to fetch alpine-keys: %w", err)
				}
				log.Warnf("ignoring missing keys: %v", err)
			}
		}

		if err := a.fetchChainguardKeys(ctx, repo); err != nil {
			return fmt.Errorf("fetching chainguard keys for %s: %w", repo, err)
		}
	}

	log.Debug("finished initializing apk database")
	return nil
}

// Resolves the possible locations of APK's DB and assures that it will exist at /lib/apk/db.
func (a *APK) resolveApkDB(ctx context.Context) error {
	log := clog.FromContext(ctx)
	log.Debug("resolving APK DB location")

	_, span := otel.Tracer("go-apk").Start(ctx, "resolveApkDB")
	defer span.End()

	// Do nothing more if /lib already points at usr/lib (absolute or relative).
	if target, err := a.fs.Readlink("/lib"); err == nil {
		// MemFS will only let Readlink succeed on a real symlink.
		// Prepend “/” and Clean to collapse things like “/../usr/lib” → “/usr/lib”.
		if path.Clean("/"+target) == "/usr/lib" {
			log.Debug("/lib is a symlink to /usr/lib")
			return nil
		}
	}

	// Do nothing more if /lib/apk already points at usr/lib/apk (absolute or relative).
	// This is the case when we start with empty layering.
	if target, err := a.fs.Readlink("/lib/apk"); err == nil {
		if path.Clean("/"+target) == "/usr/lib/apk" {
			log.Debug("/lib is a symlink to /usr/lib/apk")
			return nil
		}
	}

	// create /lib as a directory if is missing
	if _, err := a.fs.Stat("lib"); errors.Is(err, fs.ErrNotExist) {
		if err := a.fs.Mkdir("lib", 0o755); err != nil {
			return fmt.Errorf("creating lib: %w", err)
		}
		log.Debug("created /lib as a directory")
	}

	// if /lib/apk already exists, as it will with alpine's version of apk-tools, handle it
	if d, err := a.fs.Stat("lib/apk"); err == nil {
		if d.IsDir() {
			log.Debug("lib/apk is already a directory- we're likely building from alpine")
			children, err := a.fs.ReadDir("lib/apk")
			if err != nil {
				return fmt.Errorf("reading /lib/apk: %w", err)
			}
			for _, child := range children {
				if !child.IsDir() {
					return fmt.Errorf("/lib/apk contains file %s, refusing to replace", child.Name())
				}
				// check if that subdir is empty
				subents, err := a.fs.ReadDir(path.Join("lib/apk", child.Name()))
				if err != nil {
					return fmt.Errorf("reading /lib/apk/%s: %w", child.Name(), err)
				}
				if len(subents) > 0 {
					return fmt.Errorf("/lib/apk/%s is not empty, refusing to replace", child.Name())
				}
				// Delete the child directory as it is empty
				if err := a.fs.Remove(path.Join("lib/apk", child.Name())); err != nil {
					return fmt.Errorf("removing /lib/apk/%s: %w", child.Name(), err)
				}
			}
			// all children are empty dirs → safe to delete lib/apk
			if err := a.fs.Remove("lib/apk"); err != nil {
				return fmt.Errorf("removing /lib/apk: %w", err)
			}
		}
	}

	// create symlink /lib/apk → /usr/lib/apk
	if err := a.fs.Symlink("../usr/lib/apk", "lib/apk"); err != nil {
		return fmt.Errorf("creating lib/apk symlink: %w", err)
	}
	log.Debug("created symlink for lib/apk")
	return nil
}

var repoRE = regexp.MustCompile(`^http[s]?://.+\/alpine\/([^\/]+)\/[^\/]+$`)

func parseAlpineVersion(repo string) (version string, ok bool) {
	parts := repoRE.FindStringSubmatch(repo)
	if len(parts) < 2 {
		return "", false
	}
	return parts[1], true
}

// loadSystemKeyring returns the keys found in the system keyring
// directory by trying some common locations. These can be overridden
// by passing one or more directories as arguments.
func (a *APK) loadSystemKeyring(ctx context.Context, locations ...string) ([]string, error) {
	log := clog.FromContext(ctx)
	var ring []string
	if len(locations) == 0 {
		locations = []string{
			filepath.Join(DefaultSystemKeyRingPath, a.arch),
		}
	}
	for _, d := range locations {
		keyFiles, err := fs.ReadDir(a.fs, d)

		if errors.Is(err, os.ErrNotExist) {
			log.Warnf("%s doesn't exist, skipping...", d)
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("reading keyring directory: %w", err)
		}

		for _, f := range keyFiles {
			ext := filepath.Ext(f.Name())
			p := filepath.Join(d, f.Name())

			if ext == ".pub" {
				ring = append(ring, p)
			} else {
				log.Warnf("%s has invalid extension (%s), skipping...", p, ext)
			}
		}
	}
	if len(ring) > 0 {
		return ring, nil
	}
	// Return an error since reading the system keyring is the last resort
	return nil, errors.New("no suitable keyring directory found")
}

// Installs the specified keys into the APK keyring inside the build context.
func (a *APK) InitKeyring(ctx context.Context, keyFiles, extraKeyFiles []string) error {
	log := clog.FromContext(ctx)
	log.Debug("initializing apk keyring")

	ctx, span := otel.Tracer("go-apk").Start(ctx, "InitKeyring")
	defer span.End()

	if err := a.fs.MkdirAll(DefaultKeyRingPath, 0o755); err != nil {
		return fmt.Errorf("failed to make keys dir: %w", err)
	}

	if len(extraKeyFiles) > 0 {
		log.Debugf("appending %d extra keys to keyring", len(extraKeyFiles))
		keyFiles = append(keyFiles, extraKeyFiles...)
	}

	var eg errgroup.Group

	for _, element := range keyFiles {
		element := element
		eg.Go(func() error {
			log.Debugf("installing key %v", element)

			var asURL *url.URL
			var err error
			if strings.HasPrefix(element, "https://") || strings.HasPrefix(element, "http://") {
				asURL, err = url.Parse(element)
			} else {
				// Attempt to parse non-https elements into URI's so they are translated into
				// file:// URLs allowing them to parse into a url.URL{}
				asURL, err = url.Parse(string(uri.New(element)))
			}
			if err != nil {
				return fmt.Errorf("failed to parse key as URI: %w", err)
			}

			var data []byte
			switch asURL.Scheme {
			case "file": //nolint:goconst
				data, err = os.ReadFile(element)
				if err != nil {
					return fmt.Errorf("failed to read apk key: %w", err)
				}
			case "https", "http": //nolint:goconst
				client := a.client
				if a.cache != nil {
					client = a.cache.client(client, true)
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, asURL.String(), nil)
				if err != nil {
					return err
				}
				if err := a.auth.AddAuth(ctx, req); err != nil {
					return fmt.Errorf("failed to add auth to request: %w", err)
				}

				resp, err := client.Do(req)
				if err != nil {
					return fmt.Errorf("failed to fetch apk key: %w", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode < 200 || resp.StatusCode > 299 {
					return fmt.Errorf("failed to fetch apk key from %s: http response indicated error code: %d", req.Host, resp.StatusCode)
				}

				data, err = io.ReadAll(resp.Body)
				if err != nil {
					return fmt.Errorf("failed to read apk key response: %w", err)
				}
			default:
				return fmt.Errorf("scheme %s not supported", asURL.Scheme)
			}

			// #nosec G306 -- apk keyring must be publicly readable
			if err := a.fs.WriteFile(filepath.Join("etc", "apk", "keys", filepath.Base(element)), data,
				0o644); err != nil {
				return fmt.Errorf("failed to write apk key: %w", err)
			}

			return nil
		})
	}

	return eg.Wait()
}

// ResolveWorld determine the target state for the requested dependencies in /etc/apk/world. Does not install anything.
func (a *APK) ResolveWorld(ctx context.Context) (toInstall []*RepositoryPackage, conflicts []string, err error) {
	log := clog.FromContext(ctx)
	log.Debug("determining desired apk world")

	ctx, span := otel.Tracer("go-apk").Start(ctx, "ResolveWorld")
	defer span.End()

	// to fix the world, we need to:
	// 1. Get the apkIndexes for each repository for the target arch
	indexes, err := a.GetRepositoryIndexes(ctx, a.ignoreSignatures)
	if err != nil {
		return toInstall, conflicts, fmt.Errorf("error getting repository indexes: %w", err)
	}
	// debugging info, if requested
	log.Debugf("got %d indexes:\n%s", len(indexes), strings.Join(indexNames(indexes), "\n"))

	// 2. Get the dependency tree for each package from the world file
	directPkgs, err := a.GetWorld()
	if err != nil {
		return toInstall, conflicts, fmt.Errorf("error getting world packages: %w", err)
	}
	resolver := NewPkgResolver(ctx, indexes)

	// For other architectures we're building (if any), we want to disqualify any packages not present in all archs.
	allArchs := map[string][]NamedIndex{}
	for otherArch, otherAPK := range a.ByArch {
		indexes, err := otherAPK.GetRepositoryIndexes(ctx, a.ignoreSignatures)
		if err != nil {
			return toInstall, conflicts, fmt.Errorf("getting indexes for %q sibling: %w", otherArch, err)
		}
		allArchs[otherArch] = indexes
	}

	toInstall, conflicts, err = resolver.GetPackagesWithDependencies(ctx, directPkgs, allArchs)
	if err != nil {
		return
	}
	log.Debugf("got %d packages to install:\n%s", len(toInstall), strings.Join(packageRefs(toInstall), "\n"))
	return
}

func (a *APK) CalculateWorld(ctx context.Context, allpkgs []*RepositoryPackage) ([]*APKResolved, error) {
	// TODO: Consider making this configurable option.
	jobs := runtime.GOMAXPROCS(0)

	var g errgroup.Group
	g.SetLimit(jobs + 1)

	resolved := make([]*APKResolved, len(allpkgs))

	// concurrently fetch and expand all our APKs.
	for i, pkg := range allpkgs {
		i, pkg := i, pkg

		g.Go(func() error {
			expanded, err := a.expandPackage(ctx, pkg)

			if err != nil {
				return fmt.Errorf("expanding %s: %w", pkg.Name, err)
			}
			resolved[i] = NewAPKResolved(pkg, expanded)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("calculating world: %w", withCause(ctx, err))
	}

	return resolved, nil
}

// Sometimes we get an opaque error about context cancellation, and it's unclear what caused it.
// If we get something useful from ctx via context.Cause, we'll annotate err with it.
func withCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("%w: %w", err, cause)
	}

	return err
}

func (a *APK) ResolveAndCalculateWorld(ctx context.Context) ([]*APKResolved, error) {
	log := clog.FromContext(ctx)
	log.Debug("resolving and calculating 'world' (packages to install)")

	ctx, span := otel.Tracer("go-apk").Start(ctx, "CalculateWorld")
	defer span.End()

	allpkgs, _, err := a.ResolveWorld(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting package dependencies: %w", err)
	}

	return a.CalculateWorld(ctx, allpkgs)
}

// FixateWorld force apk's resolver to re-resolve the requested dependencies in /etc/apk/world.
func (a *APK) FixateWorld(ctx context.Context, sourceDateEpoch *time.Time) ([]*Package, error) {
	log := clog.FromContext(ctx)
	/*
		equivalent of: "apk fix --arch arch --root root"
		with possible options for --no-scripts, --no-cache, --update-cache

		current default is: cache=false, updateCache=true, executeScripts=false
	*/
	log.Debug("synchronizing with desired apk world")

	ctx, span := otel.Tracer("go-apk").Start(ctx, "FixateWorld")
	defer span.End()

	// to fix the world, we need to:
	// 1. Get the apkIndexes for each repository for the target arch
	allpkgs, conflicts, err := a.ResolveWorld(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting package dependencies: %w", err)
	}

	// 3. For each name on the list:
	//     a. Check if it is installed, if so, skip
	//     b. Get the .apk file
	//     c. Install the .apk file
	//     d. Update /usr/lib/apk/db/scripts.tar
	//     d. Update /usr/lib/apk/db/triggers
	//     e. Update the installed file
	for _, pkg := range conflicts {
		isInstalled, err := a.isInstalledPackage(pkg)
		if err != nil {
			return nil, fmt.Errorf("error checking if package %s is installed: %w", pkg, err)
		}
		if isInstalled {
			return nil, fmt.Errorf("cannot install due to conflict with %s", pkg)
		}
	}
	// Cast []*RepositoryPackage into []InstallablePackage.
	allInstPkgs := make([]InstallablePackage, len(allpkgs))
	for i, pkg := range allpkgs {
		allInstPkgs[i] = pkg
	}

	return a.InstallPackages(ctx, sourceDateEpoch, allInstPkgs)
}

func (a *APK) InstallPackages(ctx context.Context, sourceDateEpoch *time.Time, allpkgs []InstallablePackage) ([]*Package, error) {
	// TODO: Consider making this configurable option.
	jobs := runtime.GOMAXPROCS(0)

	var g errgroup.Group
	g.SetLimit(jobs + 1)

	expanded := make([]*expandapk.APKExpanded, len(allpkgs))

	// Track what files were installed by which packages so we can deduplicate in idb.
	allFiles := make([][]tar.Header, len(allpkgs))
	infos := make([]*Package, len(allpkgs))

	// A slice of pseudo-promises that get closed when expanded[i] is ready.
	done := make([]chan struct{}, len(allpkgs))
	for i := range allpkgs {
		done[i] = make(chan struct{})
	}

	// Kick off a goroutine that sequentially installs packages as they become ready.
	//
	// We could probably do better than this by mirroring the dependency graph or even
	// just computing non-overlapping packages based on the installed files, but we'll
	// keep this simple for now by assuming we must install in the given order exactly.
	g.Go(func() error {
		for i, ch := range done {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ch:
				exp := expanded[i]
				pkg := allpkgs[i]

				if exp == nil {
					return fmt.Errorf("expansion of %s failed", pkg)
				}

				isInstalled, err := a.isInstalledPackage(pkg.PackageName())
				if err != nil {
					return fmt.Errorf("error checking if package %s is installed: %w", pkg, err)
				}

				if isInstalled {
					continue
				}

				// The data in .PKGINFO is more complete than what is in APKINDEX.
				pkgInfo, err := packageInfo(exp)
				if err != nil {
					return fmt.Errorf("failed to read .PKGINFO for %s: %w", pkg, err)
				}
				infos[i] = pkgInfo

				installedFiles, err := a.installPackage(ctx, pkgInfo, exp, sourceDateEpoch)
				if err != nil {
					return fmt.Errorf("installing %s: %w", pkg, err)
				}

				allFiles[i] = installedFiles
			}
		}

		return nil
	})

	// Meanwhile, concurrently fetch and expand all our APKs.
	// We signal they are ready to be installed by closing done[i].
	for i, pkg := range allpkgs {
		i, pkg := i, pkg

		g.Go(func() error {
			defer func() { close(done[i]) }()
			exp, err := a.expandPackage(ctx, pkg)
			if err != nil {
				return fmt.Errorf("expanding %s: %w", pkg, err)
			}

			expanded[i] = exp

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("installing packages: %w", withCause(ctx, err))
	}

	// update the installed file
	for i, files := range allFiles {
		pkg := infos[i]

		// TODO: We currently skip over packages that are already installed.
		// I'm ignoring this for now because that isn't really a thing that can happen,
		// but if there are overlapping files from an already installed package, we should
		// modify those in the idb file.
		if pkg == nil {
			continue
		}

		// Remove any files that were overwritten by another package.
		files = slices.DeleteFunc(files, func(hdr tar.Header) bool {
			owner, ok := a.installedFiles[hdr.Name]
			if !ok {
				// Keep directories, which actually should be duplicated in the idb.
				return false
			}

			return owner != pkg
		})

		if err := a.AddInstalledPackage(pkg, files); err != nil {
			return nil, fmt.Errorf("unable to update installed file for pkg %s: %w", pkg.Name, err)
		}
	}

	// Resolve the APK DB location
	if err := a.resolveApkDB(ctx); err != nil {
		return nil, err
	}
	return infos, nil
}

type NoKeysFoundError struct {
	arch     string
	releases []string
}

func (e *NoKeysFoundError) Error() string {
	return fmt.Sprintf("no keys found for arch %s and releases %v", e.arch, e.releases)
}

// fetchAlpineKeys fetches the public keys for the repositories in the APK database.
func (a *APK) fetchAlpineKeys(ctx context.Context, alpineVersions ...string) error {
	ctx, span := otel.Tracer("go-apk").Start(ctx, "fetchAlpineKeys")
	defer span.End()

	u := alpineReleasesURL
	client := a.client
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	// NB: Not setting basic auth, since we know Alpine doesn't support it.
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch alpine releases: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unable to get alpine releases at %s: %v", u, res.Status)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read alpine releases: %w", err)
	}
	var releases Releases
	if err := json.Unmarshal(b, &releases); err != nil {
		return fmt.Errorf("failed to unmarshal alpine releases: %w", err)
	}
	var urls []string
	// now just need to get the keys for the desired architecture and releases
	for _, version := range alpineVersions {
		branch := releases.GetReleaseBranch(version)
		if branch == nil {
			continue
		}
		urls = append(urls, branch.KeysFor(a.arch, time.Now())...)
	}
	if len(urls) == 0 {
		return &NoKeysFoundError{arch: a.arch, releases: alpineVersions}
	}
	// get the keys for each URL and save them to a file with that name
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		// NB: Not setting basic auth, since we know Alpine doesn't support it.
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to fetch alpine key %s: %w", u, err)
		}
		defer res.Body.Close()
		basefilenameEscape := filepath.Base(u)
		basefilename, err := url.PathUnescape(basefilenameEscape)
		if err != nil {
			return fmt.Errorf("failed to unescape key filename %s: %w", basefilenameEscape, err)
		}
		filename := filepath.Join(keysDirPath, basefilename)
		f, err := a.fs.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("failed to open key file %s: %w", filename, err)
		}
		defer f.Close()
		if _, err := io.Copy(f, res.Body); err != nil {
			return fmt.Errorf("failed to write key file %s: %w", filename, err)
		}
	}
	return nil
}

type Key struct {
	ID    string
	Bytes []byte
}

// DiscoverKeys fetches the public keys for the repositories in the APK database using chainguard-style discovery.
func DiscoverKeys(ctx context.Context, client *http.Client, auth auth.Authenticator, repository string) ([]Key, error) {
	ctx, span := otel.Tracer("go-apk").Start(ctx, "DiscoverKeys")
	defer span.End()

	if !strings.HasPrefix(repository, "https://") && !strings.HasPrefix(repository, "http://") {
		// Ignore non-remote repositories.
		return nil, nil
	}
	asURL, err := url.Parse(strings.TrimSuffix(repository, "/") + "/apk-configuration")
	if err != nil {
		return nil, fmt.Errorf("failed to parse repository URL: %w", err)
	}

	discoveryRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, asURL.String(), nil)
	if err != nil {
		return nil, err
	}
	if err := auth.AddAuth(ctx, discoveryRequest); err != nil {
		return nil, err
	}

	discoveryResponse, err := client.Do(discoveryRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to perform key discovery: %w", err)
	}
	defer discoveryResponse.Body.Close()
	switch discoveryResponse.StatusCode {
	case http.StatusNotFound:
		// This doesn't implement Chainguard-style key discovery.
		return nil, nil

	case http.StatusOK:
		// proceed!
		break

	default:
		return nil, fmt.Errorf("chainguard key discovery was unsuccessful for repo %s: %v", repository, discoveryResponse.Status)
	}
	// Parse our the JWKS URI
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(discoveryResponse.Body).Decode(&discovery); err != nil {
		return nil, fmt.Errorf("failed to unmarshal discovery payload: %w", err)
	}

	jwksRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	if err := auth.AddAuth(ctx, jwksRequest); err != nil {
		return nil, err
	}
	jwksResponse, err := client.Do(jwksRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer jwksResponse.Body.Close()
	if jwksResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch JWKS: %v", jwksResponse.Status)
	}

	jwks := jose.JSONWebKeySet{}
	if err := json.NewDecoder(jwksResponse.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JWKS: %w", err)
	}

	keys := make([]Key, 0, len(jwks.Keys))
	for _, key := range jwks.Keys {
		if key.KeyID == "" {
			return nil, fmt.Errorf(`key missing "kid"`)
		}
		keyName := key.KeyID + ".rsa.pub"

		b, err := x509.MarshalPKIXPublicKey(key.Key.(*rsa.PublicKey))
		if err != nil {
			return nil, err
		} else if len(b) == 0 {
			return nil, fmt.Errorf("empty public key")
		}
		var buf bytes.Buffer
		if err := pem.Encode(&buf, &pem.Block{
			Type:  "PUBLIC KEY",
			Bytes: b,
		}); err != nil {
			return nil, fmt.Errorf("failed to pem encode key %s: %w", keyName, err)
		}

		keys = append(keys, Key{
			ID:    keyName,
			Bytes: buf.Bytes(),
		})
	}

	return keys, nil
}

func (a *APK) DiscoverKeys(ctx context.Context, repository string) ([]Key, error) {
	client := a.client
	if a.cache != nil {
		client = a.cache.client(client, false)

		if !a.cache.offline {
			rc := retryablehttp.NewClient()
			rc.HTTPClient = client
			rc.Logger = clog.FromContext(ctx)
			client = rc.StandardClient()
		}

		return a.cache.shared.discoverKeys.Do(repository, func() ([]Key, error) {
			return DiscoverKeys(ctx, client, a.auth, repository)
		})
	}

	return DiscoverKeys(ctx, client, a.auth, repository)
}

// fetchChainguardKeys fetches the public keys for the repositories in the APK database.
func (a *APK) fetchChainguardKeys(ctx context.Context, repository string) error {
	ctx, span := otel.Tracer("go-apk").Start(ctx, "fetchChainguardKeys")
	defer span.End()

	log := clog.FromContext(ctx)

	if !strings.HasPrefix(repository, "https://") && !strings.HasPrefix(repository, "http://") {
		log.Debugf("ignoring non-http(s) repository %s", repository)
		return nil
	}

	keys, err := a.DiscoverKeys(ctx, repository)
	if err != nil {
		log.Warnf("ignoring missing keys for %s: %v", repository, err)
	}

	for _, key := range keys {
		filename := filepath.Join(keysDirPath, key.ID)
		if err := a.fs.WriteFile(filename, key.Bytes, 0o644); err != nil {
			return fmt.Errorf("failed to write key file %s: %w", filename, err)
		}
	}
	return nil
}

func (a *APK) cachePackage(ctx context.Context, pkg InstallablePackage, exp *expandapk.APKExpanded, cacheDir string) (*expandapk.APKExpanded, error) {
	_, span := otel.Tracer("go-apk").Start(ctx, "cachePackage", trace.WithAttributes(attribute.String("package", pkg.PackageName())))
	defer span.End()

	// Rename exp's temp files to content-addressable identifiers in the cache.

	ctlHex := hex.EncodeToString(exp.ControlHash)
	ctlDst := filepath.Join(cacheDir, ctlHex+".ctl.tar.gz")

	if err := paths.AdvertiseCachedFile(exp.ControlFile, ctlDst); err != nil {
		return nil, err
	}

	exp.ControlFile = ctlDst

	if exp.SignatureFile != "" {
		sigDst := filepath.Join(cacheDir, ctlHex+".sig.tar.gz")

		if err := paths.AdvertiseCachedFile(exp.SignatureFile, sigDst); err != nil {
			return nil, err
		}

		exp.SignatureFile = sigDst
	}

	datHex := hex.EncodeToString(exp.PackageHash)
	datDst := filepath.Join(cacheDir, datHex+".dat.tar.gz")

	if err := paths.AdvertiseCachedFile(exp.PackageFile, datDst); err != nil {
		return nil, err
	}

	exp.PackageFile = datDst

	if err := exp.TarFS.Close(); err != nil {
		return nil, fmt.Errorf("closing tarfs: %w", err)
	}

	tarDst := strings.TrimSuffix(exp.PackageFile, ".gz")

	if err := paths.AdvertiseCachedFile(exp.TarFile, tarDst); err != nil {
		return nil, err
	}

	exp.TarFile = tarDst

	// Re-initialize the tarfs with the renamed file.
	// TODO: Split out the tarfs Index creation from the FS.
	// TODO: Consolidate ExpandAPK(), cachedPackage(), and cachePackage().
	data, err := exp.PackageData()
	if err != nil {
		return nil, err
	}
	info, err := data.Stat()
	if err != nil {
		return nil, err
	}
	exp.TarFS, err = tarfs.New(data, info.Size())
	if err != nil {
		return nil, err
	}

	return exp, nil
}

func (a *APK) cachedPackage(ctx context.Context, pkg InstallablePackage, cacheDir string) (*expandapk.APKExpanded, error) {
	_, span := otel.Tracer("go-apk").Start(ctx, "cachedPackage", trace.WithAttributes(attribute.String("package", pkg.PackageName())))
	defer span.End()

	chk := pkg.ChecksumString()
	if !strings.HasPrefix(chk, "Q1") {
		return nil, fmt.Errorf("unexpected checksum: %q", chk)
	}

	checksum, err := base64.StdEncoding.DecodeString(chk[2:])
	if err != nil {
		return nil, err
	}

	pkgHexSum := hex.EncodeToString(checksum)

	exp := expandapk.APKExpanded{}

	ctl := filepath.Join(cacheDir, pkgHexSum+".ctl.tar.gz")
	cf, err := os.Stat(ctl)
	if err != nil {
		return nil, err
	}
	exp.ControlFile = ctl
	exp.ControlHash = checksum
	exp.ControlSize = cf.Size()

	control, err := exp.ControlData()
	if err != nil {
		return nil, err
	}

	exp.ControlFS, err = tarfs.New(bytes.NewReader(control), int64(len(control)))
	if err != nil {
		return nil, fmt.Errorf("indexing %q: %w", exp.ControlFile, err)
	}

	exp.Size += cf.Size()

	sig := filepath.Join(cacheDir, pkgHexSum+".sig.tar.gz")
	sf, err := os.Stat(sig)
	if err == nil {
		exp.SignatureFile = sig
		exp.Signed = true
		exp.Size += sf.Size()
		exp.SignatureSize = sf.Size()
		signatureData, err := os.ReadFile(sig)
		if err != nil {
			return nil, err
		}
		signatureHash := sha1.Sum(signatureData) //nolint:gosec // this is what apk tools is using
		exp.SignatureHash = signatureHash[:]
	}

	f, err := os.Open(ctl)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	datahash, err := a.datahash(f)
	if err != nil {
		return nil, fmt.Errorf("datahash for %s: %w", pkg, err)
	}

	dat := filepath.Join(cacheDir, datahash+".dat.tar.gz")
	df, err := os.Stat(dat)
	if err != nil {
		return nil, err
	}
	exp.PackageFile = dat
	exp.PackageSize = df.Size()
	exp.Size += df.Size()

	exp.PackageHash, err = hex.DecodeString(datahash)
	if err != nil {
		return nil, err
	}

	exp.TarFile = strings.TrimSuffix(exp.PackageFile, ".gz")
	data, err := exp.PackageData()
	if err != nil {
		return nil, err
	}
	info, err := data.Stat()
	if err != nil {
		return nil, err
	}
	exp.TarFS, err = tarfs.New(data, info.Size())
	if err != nil {
		return nil, err
	}

	return &exp, nil
}

type apkResult struct {
	exp *expandapk.APKExpanded
	err error
}

type apkCache struct {
	// url -> *sync.Once
	onces sync.Map

	// url -> apkResult
	resps sync.Map
}

func (c *apkCache) get(ctx context.Context, a *APK, pkg InstallablePackage) (*expandapk.APKExpanded, error) {
	u := pkg.URL()
	// Do all the expensive things inside the once.
	once, _ := c.onces.LoadOrStore(u, &sync.Once{})
	once.(*sync.Once).Do(func() {
		exp, err := expandPackage(ctx, a, pkg)
		c.resps.Store(u, apkResult{
			exp: exp,
			err: err,
		})
	})

	v, ok := c.resps.Load(u)
	if !ok {
		panic(fmt.Errorf("did not see apk %q after writing it", u))
	}

	result := v.(apkResult)
	return result.exp, result.err
}

func (a *APK) expandPackage(ctx context.Context, pkg InstallablePackage) (*expandapk.APKExpanded, error) {
	if a.cache == nil {
		// If we don't have a cache configured, don't use the global cache.
		// Calling APKExpanded.Close() will clean up a tempdir.
		// This is fine when we have a cache because we move all the backing files into the cache.
		// This is not fine when we don't have a cache because the tempdir contains all our state.
		return expandPackage(ctx, a, pkg)
	}

	return globalApkCache.get(ctx, a, pkg)
}

func expandPackage(ctx context.Context, a *APK, pkg InstallablePackage) (*expandapk.APKExpanded, error) {
	log := clog.FromContext(ctx)
	ctx, span := otel.Tracer("go-apk").Start(ctx, "expandPackage", trace.WithAttributes(attribute.String("package", pkg.PackageName())))
	defer span.End()

	cacheDir := ""
	if a.cache != nil {
		var err error
		cacheDir, err = cacheDirForPackage(a.cache.dir, pkg)
		if err != nil {
			return nil, err
		}

		exp, err := a.cachedPackage(ctx, pkg, cacheDir)
		if err == nil {
			log.Debugf("cache hit (%s)", pkg.PackageName())
			return exp, nil
		}

		log.Debugf("cache miss (%s): %v", pkg.PackageName(), err)

		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return nil, fmt.Errorf("unable to create cache directory %q: %w", cacheDir, err)
		}
	}

	rc, err := a.FetchPackage(ctx, pkg)
	if err != nil {
		return nil, fmt.Errorf("fetching package %q: %w", pkg.PackageName(), err)
	}
	defer rc.Close()

	exp, err := expandapk.ExpandApk(ctx, rc, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("expanding %s: %w", pkg.PackageName(), err)
	}

	// If we don't have a cache, we're done.
	if a.cache == nil {
		return exp, nil
	}

	return a.cachePackage(ctx, pkg, exp, cacheDir)
}

func packageAsURI(pkg LocatablePackage) (uri.URI, error) {
	u := pkg.URL()

	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return uri.Parse(u)
	}

	return uri.New(u), nil
}

func packageAsURL(pkg LocatablePackage) (*url.URL, error) {
	asURI, err := packageAsURI(pkg)
	if err != nil {
		return nil, err
	}

	return url.Parse(string(asURI))
}

func (a *APK) FetchPackage(ctx context.Context, pkg FetchablePackage) (io.ReadCloser, error) {
	log := clog.FromContext(ctx)
	log.Debugf("fetching %s", pkg)

	ctx, span := otel.Tracer("go-apk").Start(ctx, "fetchPackage", trace.WithAttributes(attribute.String("package", pkg.PackageName())))
	defer span.End()

	u := pkg.URL()

	// Normalize the repo as a URI, so that local paths
	// are translated into file:// URLs, allowing them to be parsed
	// into a url.URL{}.
	asURL, err := packageAsURL(pkg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse package as URL: %w", err)
	}

	switch asURL.Scheme {
	case "file":
		f, err := os.Open(u)
		if err != nil {
			return nil, fmt.Errorf("failed to read repository package apk %s: %w", u, err)
		}
		return f, nil
	case "https", "http":
		client := a.client
		if a.cache != nil {
			client = a.cache.client(client, false)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if err := a.auth.AddAuth(ctx, req); err != nil {
			return nil, err
		}

		// This will return a body that retries requests using Range requests if Read() hits an error.
		rrt := newRangeRetryTransport(ctx, client)
		res, err := rrt.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("unable to get package apk at %s: %w", u, err)
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("unable to get package apk at %s: %v", u, res.Status)
		}
		return res.Body, nil
	default:
		return nil, fmt.Errorf("repository scheme %s not supported", asURL.Scheme)
	}
}

type WriteHeaderer interface {
	WriteHeader(hdr tar.Header, tfs fs.FS, pkg *Package) (bool, error)
}

func packageInfo(exp *expandapk.APKExpanded) (*Package, error) {
	f, err := exp.ControlFS.Open(".PKGINFO")
	if err != nil {
		return nil, fmt.Errorf("opening .PKGINFO in %s: %w", exp.ControlFile, err)
	}
	defer f.Close()

	cfg, err := ini.ShadowLoad(f)
	if err != nil {
		return nil, fmt.Errorf("ini.ShadowLoad(): %w", err)
	}

	pkg := new(Package)
	if err = cfg.MapTo(pkg); err != nil {
		return nil, fmt.Errorf("cfg.MapTo(): %w", err)
	}
	pkg.BuildTime = time.Unix(pkg.BuildDate, 0).UTC()
	pkg.InstalledSize = pkg.Size
	pkg.Size = uint64(exp.Size)
	pkg.Checksum = exp.ControlHash

	return pkg, nil
}

// installPackage installs a single package and updates installed db.
func (a *APK) installPackage(ctx context.Context, pkg *Package, expanded *expandapk.APKExpanded, sourceDateEpoch *time.Time) ([]tar.Header, error) {
	log := clog.FromContext(ctx)
	log.Infof("installing %s (%s)", pkg.Name, pkg.Version)

	// We don't want to call `defer expanded.Close()` to to remove tempDir because our
	// cached files are advertised by symlinks pointing into them.
	//
	// This is not a big deal because the temp files if not referred by
	// a symlink will be cleaned up anyway.

	ctx, span := otel.Tracer("go-apk").Start(ctx, "installPackage", trace.WithAttributes(attribute.String("package", pkg.Name)))
	defer span.End()

	var (
		err            error
		installedFiles []tar.Header
	)

	if wh, ok := a.fs.(WriteHeaderer); ok {
		installedFiles, err = a.lazilyInstallAPKFiles(ctx, wh, expanded.TarFS, pkg)
		if err != nil {
			return nil, fmt.Errorf("unable to install files for pkg %s: %w", pkg.Name, err)
		}
	} else {
		packageData, err := expanded.PackageData()
		if err != nil {
			return nil, fmt.Errorf("opening package file %q: %w", expanded.PackageFile, err)
		}
		defer packageData.Close()

		installedFiles, err = a.installAPKFiles(ctx, packageData, pkg)
		if err != nil {
			return nil, fmt.Errorf("unable to install files for pkg %s: %w", pkg.Name, err)
		}
	}

	// update the scripts.tar
	controlData, err := os.Open(expanded.ControlFile)
	if err != nil {
		return nil, fmt.Errorf("opening control file %q: %w", expanded.ControlFile, err)
	}
	defer controlData.Close()

	if err := a.updateScriptsTar(pkg, controlData, sourceDateEpoch); err != nil {
		return nil, fmt.Errorf("unable to update scripts.tar for pkg %s: %w", pkg.Name, err)
	}

	// update the triggers
	if _, err := controlData.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("unable to seek to start of control data for pkg %s: %w", pkg.Name, err)
	}
	if err := a.updateTriggers(pkg, controlData); err != nil {
		return nil, fmt.Errorf("unable to update triggers for pkg %s: %w", pkg.Name, err)
	}

	return installedFiles, nil
}

func (a *APK) datahash(controlTarGz io.Reader) (string, error) {
	values, err := a.controlValue(controlTarGz, "datahash")
	if err != nil {
		return "", fmt.Errorf("reading datahash from control: %w", err)
	}

	if len(values) != 1 {
		return "", fmt.Errorf("saw %d datahash values", len(values))
	}

	return values[0], nil
}

func packageRefs(pkgs []*RepositoryPackage) []string {
	names := make([]string, len(pkgs))
	for i, pkg := range pkgs {
		names[i] = fmt.Sprintf("%s (%s) %s", pkg.Name, pkg.Version, pkg.URL())
	}
	return names
}
