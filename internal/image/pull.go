// Package image pulls container images from a Docker-Hub-compatible OCI
// registry (v2 Distribution API) and unpacks their layers into a rootfs
// directory usable by the container package.
//
// This only implements what's needed to pull public images anonymously
// from Docker Hub: token auth, manifest (and manifest-list) resolution,
// and layer download + extraction. It does not implement pushing,
// private registries requiring real credentials, or OCI whiteout-file
// deletion semantics (see the note on that in extractLayer below).
package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	registryBase = "https://registry-1.docker.io"
	authBase     = "https://auth.docker.io/token"

	mediaTypeManifestV2   = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex     = "application/vnd.oci.image.index.v1+json"
)

// Ref is a parsed image reference like "alpine:latest" or "library/ubuntu:22.04".
type Ref struct {
	Repository string // e.g. "library/alpine"
	Tag        string // e.g. "latest"
}

// ParseRef parses a Docker-Hub-style image reference. Official images
// (no "/" in the name) are expanded to "library/<name>", matching how
// Docker Hub actually namespaces them.
func ParseRef(s string) Ref {
	repo, tag := s, "latest"
	if idx := strings.LastIndex(s, ":"); idx != -1 && !strings.Contains(s[idx:], "/") {
		repo, tag = s[:idx], s[idx+1:]
	}
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return Ref{Repository: repo, Tag: tag}
}

type manifestList struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

type manifest struct {
	MediaType string `json:"mediaType"`
	Layers    []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// Pull downloads the image named by ref and unpacks all of its layers,
// in order, into destDir -- which becomes a usable container rootfs.
func Pull(refStr, destDir string) error {
	ref := ParseRef(refStr)

	token, err := getToken(ref.Repository)
	if err != nil {
		return fmt.Errorf("authenticating with registry: %w", err)
	}

	m, err := getManifest(ref, token)
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("creating destination dir: %w", err)
	}

	for i, layer := range m.Layers {
		fmt.Printf("myrun: pulling layer %d/%d (%s, %.1f MB)\n",
			i+1, len(m.Layers), shortDigest(layer.Digest), float64(layer.Size)/(1024*1024))
		if err := downloadAndExtractLayer(ref.Repository, layer.Digest, token, destDir); err != nil {
			return fmt.Errorf("layer %s: %w", shortDigest(layer.Digest), err)
		}
	}

	return nil
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// getToken obtains an anonymous pull token from Docker Hub's auth service.
// Public images don't need real credentials for this -- anonymous tokens
// are issued for read-only pull scope.
func getToken(repository string) (string, error) {
	url := fmt.Sprintf("%s?service=registry.docker.io&scope=repository:%s:pull", authBase, repository)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("auth service returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("empty token in auth response")
	}
	return result.Token, nil
}

// getManifest fetches the manifest for ref, resolving a manifest list
// (multi-architecture index) down to the linux/amd64 manifest if needed.
func getManifest(ref Ref, token string) (*manifest, error) {
	acceptTypes := strings.Join([]string{
		mediaTypeManifestV2, mediaTypeManifestList, mediaTypeOCIManifest, mediaTypeOCIIndex,
	}, ",")

	body, mediaType, err := fetchManifestRaw(ref.Repository, ref.Tag, token, acceptTypes)
	if err != nil {
		return nil, err
	}

	if mediaType == mediaTypeManifestList || mediaType == mediaTypeOCIIndex {
		var list manifestList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decoding manifest list: %w", err)
		}
		var chosenDigest string
		for _, entry := range list.Manifests {
			if entry.Platform.OS == "linux" && entry.Platform.Architecture == "amd64" {
				chosenDigest = entry.Digest
				break
			}
		}
		if chosenDigest == "" {
			return nil, fmt.Errorf("no linux/amd64 manifest found in manifest list")
		}
		body, _, err = fetchManifestRaw(ref.Repository, chosenDigest, token, acceptTypes)
		if err != nil {
			return nil, fmt.Errorf("fetching linux/amd64 manifest: %w", err)
		}
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("manifest has no layers")
	}
	return &m, nil
}

func fetchManifestRaw(repository, ref, token, acceptTypes string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", registryBase, repository, ref)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", acceptTypes)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("registry returned %d for manifest %s: %s", resp.StatusCode, ref, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// downloadAndExtractLayer streams a single layer blob (gzipped tar) and
// extracts it directly into destDir without buffering the whole thing in
// memory.
func downloadAndExtractLayer(repository, digest, token, destDir string) error {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", registryBase, repository, digest)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry returned %d for blob: %s", resp.StatusCode, string(body))
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gzr.Close()

	return extractLayer(gzr, destDir)
}

// extractLayer unpacks a tar stream into destDir.
//
// Known limitation: this does not implement OCI whiteout-file semantics
// (files named ".wh.<name>" in a layer mean "delete <name> from earlier
// layers"). For simple images built as a single layer or a few purely
// additive layers (true of most minimal base images like alpine), this
// doesn't matter. For images whose layers actually delete/replace files
// from earlier layers, extraction will leave stale files behind instead
// of properly removing them. Worth fixing if a pulled image behaves
// unexpectedly.
func extractLayer(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			f.Close()
		case tar.TypeSymlink:
			os.Remove(target) // symlink() fails if target already exists
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				// Non-fatal: some layers contain symlinks to paths that
				// don't matter for a basic chroot rootfs. Log and continue
				// rather than aborting the whole pull.
				fmt.Fprintf(os.Stderr, "myrun: warning: could not create symlink %s -> %s: %v\n", target, hdr.Linkname, err)
			}
		case tar.TypeLink:
			hardTarget := filepath.Join(destDir, filepath.Clean("/"+hdr.Linkname))
			if err := os.Link(hardTarget, target); err != nil {
				fmt.Fprintf(os.Stderr, "myrun: warning: could not create hardlink %s -> %s: %v\n", target, hardTarget, err)
			}
		default:
			// Device files, fifos, etc. Skip silently -- not needed for
			// a basic chroot rootfs and often require privileges that
			// may not be available.
		}
	}
}