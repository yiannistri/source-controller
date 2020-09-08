/*
Copyright 2020 The Flux CD contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fluxcd/pkg/lockedfile"

	sourcev1 "github.com/fluxcd/source-controller/api/v1alpha1"
	"github.com/fluxcd/source-controller/internal/fs"
)

const (
	excludeFile = ".sourceignore"
	excludeVCS  = ".git/,.gitignore,.gitmodules,.gitattributes"
	excludeExt  = "*.jpg,*.jpeg,*.gif,*.png,*.wmv,*.flv,*.tar.gz,*.zip"
)

// Storage manages artifacts
type Storage struct {
	// BasePath is the local directory path where the source artifacts are stored.
	BasePath string `json:"basePath"`

	// Hostname is the file server host name used to compose the artifacts URIs.
	Hostname string `json:"hostname"`

	// Timeout for artifacts operations
	Timeout time.Duration `json:"timeout"`
}

// NewStorage creates the storage helper for a given path and hostname
func NewStorage(basePath string, hostname string, timeout time.Duration) (*Storage, error) {
	if f, err := os.Stat(basePath); os.IsNotExist(err) || !f.IsDir() {
		return nil, fmt.Errorf("invalid dir path: %s", basePath)
	}

	return &Storage{
		BasePath: basePath,
		Hostname: hostname,
		Timeout:  timeout,
	}, nil
}

// ArtifactFor returns an artifact for the v1alpha1.Source.
func (s *Storage) ArtifactFor(kind string, metadata metav1.Object, fileName, revision, checksum string) sourcev1.Artifact {
	path := sourcev1.ArtifactPath(kind, metadata.GetNamespace(), metadata.GetName(), fileName)
	url := fmt.Sprintf("http://%s/%s", s.Hostname, path)

	return sourcev1.Artifact{
		Path:           path,
		URL:            url,
		Revision:       revision,
		Checksum:       checksum,
		LastUpdateTime: metav1.Now(),
	}
}

// MkdirAll calls os.MkdirAll for the given v1alpha1.Artifact base dir.
func (s *Storage) MkdirAll(artifact sourcev1.Artifact) error {
	dir := filepath.Dir(s.LocalPath(artifact))
	return os.MkdirAll(dir, 0777)
}

// RemoveAll calls os.RemoveAll for the given v1alpha1.Artifact base dir.
func (s *Storage) RemoveAll(artifact sourcev1.Artifact) error {
	dir := filepath.Dir(s.LocalPath(artifact))
	return os.RemoveAll(dir)
}

// RemoveAllButCurrent removes all files for the given v1alpha1.Artifact base dir,
// excluding the current one.
func (s *Storage) RemoveAllButCurrent(artifact sourcev1.Artifact) error {
	localPath := s.LocalPath(artifact)
	dir := filepath.Dir(localPath)
	var errors []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			errors = append(errors, err.Error())
			return nil
		}

		if path != localPath && !info.IsDir() && info.Mode()&os.ModeSymlink != os.ModeSymlink {
			if err := os.Remove(path); err != nil {
				errors = append(errors, info.Name())
			}
		}
		return nil
	})

	if len(errors) > 0 {
		return fmt.Errorf("failed to remove files: %s", strings.Join(errors, " "))
	}
	return nil
}

// ArtifactExist returns a boolean indicating whether the v1alpha1.Artifact exists in storage
// and is a regular file.
func (s *Storage) ArtifactExist(artifact sourcev1.Artifact) bool {
	fi, err := os.Lstat(s.LocalPath(artifact))
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// Archive atomically creates a tar.gz to the v1alpha1.Artifact path from the given dir,
// excluding any VCS specific files and directories, or any of the excludes defined in
// the excludeFiles.
func (s *Storage) Archive(artifact sourcev1.Artifact, dir string, spec sourcev1.GitRepositorySpec) (err error) {
	if _, err := os.Stat(dir); err != nil {
		return err
	}

	ps, err := loadExcludePatterns(dir, spec)
	if err != nil {
		return err
	}
	matcher := gitignore.NewMatcher(ps)

	localPath := s.LocalPath(artifact)
	tmpGzFile, err := ioutil.TempFile(filepath.Split(localPath))
	if err != nil {
		return err
	}
	tmpName := tmpGzFile.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	gw := gzip.NewWriter(tmpGzFile)
	tw := tar.NewWriter(gw)
	if err := writeToArchiveExcludeMatches(dir, matcher, tw); err != nil {
		tw.Close()
		gw.Close()
		tmpGzFile.Close()
		return err
	}

	if err := tw.Close(); err != nil {
		gw.Close()
		tmpGzFile.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		tmpGzFile.Close()
		return err
	}
	if err := tmpGzFile.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}

	return fs.RenameWithFallback(tmpName, localPath)
}

// writeToArchiveExcludeMatches walks over the given dir and writes any regular file that does
// not match the given gitignore.Matcher.
func writeToArchiveExcludeMatches(dir string, matcher gitignore.Matcher, writer *tar.Writer) error {
	fn := func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Ignore anything that is not a file (directories, symlinks)
		if !fi.Mode().IsRegular() {
			return nil
		}

		// Ignore excluded extensions and files
		if matcher.Match(strings.Split(p, "/"), false) {
			return nil
		}

		header, err := tar.FileInfoHeader(fi, p)
		if err != nil {
			return err
		}
		// The name needs to be modified to maintain directory structure
		// as tar.FileInfoHeader only has access to the base name of the file.
		// Ref: https://golang.org/src/archive/tar/common.go?#L626
		relFilePath := p
		if filepath.IsAbs(dir) {
			relFilePath, err = filepath.Rel(dir, p)
			if err != nil {
				return err
			}
		}
		header.Name = relFilePath

		if err := writer.WriteHeader(header); err != nil {
			return err
		}

		f, err := os.Open(p)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(writer, f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	return filepath.Walk(dir, fn)
}

// AtomicWriteFile atomically writes a file to the v1alpha1.Artifact Path.
func (s *Storage) AtomicWriteFile(artifact sourcev1.Artifact, reader io.Reader, mode os.FileMode) (err error) {
	localPath := s.LocalPath(artifact)
	tmpFile, err := ioutil.TempFile(filepath.Split(localPath))
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return fs.RenameWithFallback(tmpName, localPath)
}

// Symlink creates or updates a symbolic link for the given artifact
// and returns the URL for the symlink.
func (s *Storage) Symlink(artifact sourcev1.Artifact, linkName string) (string, error) {
	localPath := s.LocalPath(artifact)
	dir := filepath.Dir(localPath)
	link := filepath.Join(dir, linkName)
	tmpLink := link + ".tmp"

	if err := os.Remove(tmpLink); err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if err := os.Symlink(localPath, tmpLink); err != nil {
		return "", err
	}

	if err := os.Rename(tmpLink, link); err != nil {
		return "", err
	}

	parts := strings.Split(artifact.URL, "/")
	url := strings.Replace(artifact.URL, parts[len(parts)-1], linkName, 1)
	return url, nil
}

// Checksum returns the SHA1 checksum for the data of the given io.Reader as a string.
func (s *Storage) Checksum(reader io.Reader) string {
	h := sha1.New()
	_, _ = io.Copy(h, reader)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Lock creates a file lock for the given v1alpha1.Artifact.
func (s *Storage) Lock(artifact sourcev1.Artifact) (unlock func(), err error) {
	lockFile := s.LocalPath(artifact) + ".lock"
	mutex := lockedfile.MutexAt(lockFile)
	return mutex.Lock()
}

// LocalPath returns the local path of the given artifact (that is: relative to
// the Storage.BasePath).
func (s *Storage) LocalPath(artifact sourcev1.Artifact) string {
	if artifact.Path == "" {
		return ""
	}
	return filepath.Join(s.BasePath, artifact.Path)
}

// getPatterns collects ignore patterns from the given reader and returns them
// as a gitignore.Pattern slice.
func getPatterns(reader io.Reader, path []string) []gitignore.Pattern {
	var ps []gitignore.Pattern
	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		s := scanner.Text()
		if !strings.HasPrefix(s, "#") && len(strings.TrimSpace(s)) > 0 {
			ps = append(ps, gitignore.ParsePattern(s, path))
		}
	}

	return ps
}

// loadExcludePatterns loads the excluded patterns from sourceignore or other
// sources.
func loadExcludePatterns(dir string, spec sourcev1.GitRepositorySpec) ([]gitignore.Pattern, error) {
	path := strings.Split(dir, "/")

	var ps []gitignore.Pattern
	for _, p := range strings.Split(excludeVCS, ",") {
		ps = append(ps, gitignore.ParsePattern(p, path))
	}

	if spec.Ignore == nil {
		for _, p := range strings.Split(excludeExt, ",") {
			ps = append(ps, gitignore.ParsePattern(p, path))
		}

		if f, err := os.Open(filepath.Join(dir, excludeFile)); err == nil {
			defer f.Close()
			ps = append(ps, getPatterns(f, path)...)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		ps = append(ps, getPatterns(bytes.NewBufferString(*spec.Ignore), path)...)
	}

	return ps, nil
}
