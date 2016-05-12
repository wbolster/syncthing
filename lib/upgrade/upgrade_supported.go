// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// +build !noupgrade

package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/signature"
)

const DisabledByCompilation = false

const (
	// Current binary size hovers around 10 MB. We give it some room to grow
	// and say that we never expect the binary to be larger than 64 MB.
	maxBinarySize = 64 << 20 // 64 MiB

	// The max expected size of the signature file.
	maxSignatureSize = 1 << 10 // 1 KiB

	// We set the same limit on the archive. The binary will compress and we
	// include some other stuff - currently the release archive size is
	// around 6 MB.
	maxArchiveSize = maxBinarySize

	// When looking through the archive for the binary and signature, stop
	// looking once we've searched this many files.
	maxArchiveMembers = 100

	// Archive reads, or metadata checks, that take longer than this will be
	// rejected.
	readTimeout = 30 * time.Minute

	// The limit on the size of metadata that we accept.
	maxMetadataSize = 100 << 10 // 100 KiB
)

// This is an HTTP/HTTPS client that does *not* perform certificate
// validation. We do this because some systems where Syncthing runs have
// issues with old or missing CA roots. It doesn't actually matter that we
// load the upgrade insecurely as we verify an ECDSA signature of the actual
// binary contents before accepting the upgrade.
var insecureHTTP = &http.Client{
	Timeout: readTimeout,
	Transport: &http.Transport{
		Dial:  dialer.Dial,
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	},
}

// FetchLatestReleases returns the latest releases, including prereleases or
// not depending on the argument
func FetchLatestReleases(releasesURL, version string) []Release {
	resp, err := insecureHTTP.Get(releasesURL)
	if err != nil {
		l.Infoln("Couldn't fetch release information:", err)
		return nil
	}
	if resp.StatusCode > 299 {
		l.Infoln("API call returned HTTP error:", resp.Status)
		return nil
	}

	var rels []Release
	json.NewDecoder(io.LimitReader(resp.Body, maxMetadataSize)).Decode(&rels)
	resp.Body.Close()

	return rels
}

type SortByRelease []Release

func (s SortByRelease) Len() int {
	return len(s)
}
func (s SortByRelease) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s SortByRelease) Less(i, j int) bool {
	return CompareVersions(s[i].Tag, s[j].Tag) > 0
}

func LatestRelease(releasesURL, version string) (Release, error) {
	rels := FetchLatestReleases(releasesURL, version)
	return SelectLatestRelease(version, rels)
}

func SelectLatestRelease(version string, rels []Release) (Release, error) {
	if len(rels) == 0 {
		return Release{}, ErrNoVersionToSelect
	}

	sort.Sort(SortByRelease(rels))
	// Check for a beta build
	beta := strings.Contains(version, "-")

	for _, rel := range rels {
		if rel.Prerelease && !beta {
			continue
		}
		for _, asset := range rel.Assets {
			assetName := path.Base(asset.Name)
			// Check for the architecture
			expectedRelease := releaseName(rel.Tag)
			l.Debugf("expected release asset %q", expectedRelease)
			l.Debugln("considering release", assetName)
			if strings.HasPrefix(assetName, expectedRelease) {
				return rel, nil
			}
		}
	}
	return Release{}, ErrNoReleaseDownload
}

// Upgrade to the given release, saving the previous binary with a ".old" extension.
func upgradeTo(binary string, rel Release) error {
	expectedRelease := releaseName(rel.Tag)
	l.Debugf("expected release asset %q", expectedRelease)
	for _, asset := range rel.Assets {
		assetName := path.Base(asset.Name)
		l.Debugln("considering release", assetName)

		if strings.HasPrefix(assetName, expectedRelease) {
			return upgradeToURL(assetName, binary, asset.URL)
		}
	}

	return ErrNoReleaseDownload
}

// Upgrade to the given release, saving the previous binary with a ".old" extension.
func upgradeToURL(archiveName, binary string, url string) error {
	fname, err := readRelease(archiveName, filepath.Dir(binary), url)
	if err != nil {
		return err
	}

	old := binary + ".old"
	os.Remove(old)
	err = os.Rename(binary, old)
	if err != nil {
		return err
	}
	err = os.Rename(fname, binary)
	if err != nil {
		return err
	}
	return nil
}

func readRelease(archiveName, dir, url string) (string, error) {
	l.Debugf("loading %q", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Add("Accept", "application/octet-stream")
	resp, err := insecureHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	switch runtime.GOOS {
	case "windows":
		return readZip(archiveName, dir, io.LimitReader(resp.Body, maxArchiveSize))
	default:
		return readTarGz(archiveName, dir, io.LimitReader(resp.Body, maxArchiveSize))
	}
}

func readTarGz(archiveName, dir string, r io.Reader) (string, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}

	tr := tar.NewReader(gr)

	var tempName string
	var sig []byte

	// Iterate through the files in the archive.
	i := 0
	for {
		if i >= maxArchiveMembers {
			break
		}
		i++

		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Size > maxBinarySize {
			// We don't even want to try processing or skipping over files
			// that are too large.
			break
		}

		err = archiveFileVisitor(dir, &tempName, &sig, hdr.Name, tr)
		if err != nil {
			return "", err
		}

		if tempName != "" && sig != nil {
			break
		}
	}

	if err := verifyUpgrade(archiveName, tempName, sig); err != nil {
		return "", err
	}

	return tempName, nil
}

func readZip(archiveName, dir string, r io.Reader) (string, error) {
	body, err := ioutil.ReadAll(r)
	if err != nil {
		return "", err
	}

	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", err
	}

	var tempName string
	var sig []byte

	// Iterate through the files in the archive.
	i := 0
	for _, file := range archive.File {
		if i >= maxArchiveMembers {
			break
		}
		i++

		if file.UncompressedSize64 > maxBinarySize {
			// We don't even want to try processing or skipping over files
			// that are too large.
			break
		}

		inFile, err := file.Open()
		if err != nil {
			return "", err
		}

		err = archiveFileVisitor(dir, &tempName, &sig, file.Name, inFile)
		inFile.Close()
		if err != nil {
			return "", err
		}

		if tempName != "" && sig != nil {
			break
		}
	}

	if err := verifyUpgrade(archiveName, tempName, sig); err != nil {
		return "", err
	}

	return tempName, nil
}

// archiveFileVisitor is called for each file in an archive. It may set
// tempFile and signature.
func archiveFileVisitor(dir string, tempFile *string, signature *[]byte, archivePath string, filedata io.Reader) error {
	var err error
	filename := path.Base(archivePath)
	archiveDir := path.Dir(archivePath)
	l.Debugf("considering file %s", archivePath)
	switch filename {
	case "syncthing", "syncthing.exe":
		archiveDirs := strings.Split(archiveDir, "/")
		if len(archiveDirs) > 1 {
			// Don't consider "syncthing" files found too deeply, as they may be
			// other things.
			return nil
		}
		l.Debugf("found upgrade binary %s", archivePath)
		*tempFile, err = writeBinary(dir, io.LimitReader(filedata, maxBinarySize))
		if err != nil {
			return err
		}

	case "release.sig":
		l.Debugf("found signature %s", archivePath)
		*signature, err = ioutil.ReadAll(io.LimitReader(filedata, maxSignatureSize))
		if err != nil {
			return err
		}
	}

	return nil
}

func verifyUpgrade(archiveName, tempName string, sig []byte) error {
	if tempName == "" {
		return fmt.Errorf("no upgrade found")
	}
	if sig == nil {
		return fmt.Errorf("no signature found")
	}

	l.Debugf("checking signature\n%s", sig)

	fd, err := os.Open(tempName)
	if err != nil {
		return err
	}

	// Create a new reader that will serve reads from, in order:
	//
	// - the archive name ("syncthing-linux-amd64-v0.13.0-beta.4.tar.gz")
	//   followed by a newline
	//
	// - the temp file contents
	//
	// We then verify the release signature against the contents of this
	// multireader. This ensures that it is not only a bonafide syncthing
	// binary, but it it also of exactly the platform and version we expect.

	mr := io.MultiReader(bytes.NewBufferString(archiveName+"\n"), fd)
	err = signature.Verify(SigningKey, sig, mr)
	fd.Close()

	if err != nil {
		os.Remove(tempName)
		return err
	}

	return nil
}

func writeBinary(dir string, inFile io.Reader) (filename string, err error) {
	// Write the binary to a temporary file.

	outFile, err := ioutil.TempFile(dir, "syncthing")
	if err != nil {
		return "", err
	}

	_, err = io.Copy(outFile, inFile)
	if err != nil {
		os.Remove(outFile.Name())
		return "", err
	}

	err = outFile.Close()
	if err != nil {
		os.Remove(outFile.Name())
		return "", err
	}

	err = os.Chmod(outFile.Name(), os.FileMode(0755))
	if err != nil {
		os.Remove(outFile.Name())
		return "", err
	}

	return outFile.Name(), nil
}