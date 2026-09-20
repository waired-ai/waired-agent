package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mediaTypeModel is the manifest layer that holds the weights. The other
// layers a tag carries — projector, license, params, template — are not what
// a GGUF edit is about.
const mediaTypeModel = "application/vnd.ollama.image.model"

// ErrNoModelLayer is returned for a manifest that carries no weights layer.
var ErrNoModelLayer = errors.New("download: manifest has no model layer")

// ollamaName is a tag split the way ollama splits it.
type ollamaName struct{ host, namespace, model, tag string }

// parseOllamaTag splits a tag into the four parts whose join is its path
// under <models>/manifests. It reproduces ollama v0.34.0's
// types/model.ParseName: cut the tag at the LAST ":" but only when that
// colon comes after the last "/", then cut host / namespace / model from the
// right, then fill what is missing from the defaults.
//
// Reproduced rather than called because ollama is a binary this product
// runs, not a library it imports. Checked against the two shapes the shipped
// catalog uses, on the reference host's own store:
//
//	qwen3.6:35b-a3b-mtp-q4_K_M
//	  -> registry.ollama.ai/library/qwen3.6/35b-a3b-mtp-q4_K_M
//	hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL
//	  -> hf.co/unsloth/Qwen3.8-27B-GGUF/UD-Q3_K_XL
func parseOllamaTag(tag string) (ollamaName, error) {
	n := ollamaName{host: "registry.ollama.ai", namespace: "library", tag: "latest"}
	s := strings.TrimSpace(tag)
	if s == "" {
		return n, errors.New("download: empty tag")
	}
	if strings.LastIndex(s, ":") > strings.LastIndex(s, "/") {
		i := strings.LastIndex(s, ":")
		s, n.tag = s[:i], s[i+1:]
	}
	cutLast := func(s, sep string) (before, after string, ok bool) {
		i := strings.LastIndex(s, sep)
		if i < 0 {
			return s, "", false
		}
		return s[:i], s[i+len(sep):], true
	}
	rest, model, ok := cutLast(s, "/")
	if !ok {
		n.model = s
		return n, n.validate(tag)
	}
	n.model = model
	rest, ns, ok := cutLast(rest, "/")
	if !ok {
		n.namespace = rest
		return n, n.validate(tag)
	}
	n.namespace = ns
	// Any scheme prefix is not part of the on-disk path.
	if _, host, found := strings.Cut(rest, "://"); found {
		n.host = host
	} else {
		n.host = rest
	}
	return n, n.validate(tag)
}

func (n ollamaName) validate(tag string) error {
	for _, p := range []string{n.host, n.namespace, n.model, n.tag} {
		// A part that is empty, or that would climb out of the store, means
		// a tag this code does not understand — better to refuse than to
		// edit a file somewhere else entirely.
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) {
			return fmt.Errorf("download: tag %q does not split into a store path", tag)
		}
	}
	return nil
}

// ManifestPath is where tag's local manifest lives inside modelsDir.
func ManifestPath(modelsDir, tag string) (string, error) {
	n, err := parseOllamaTag(tag)
	if err != nil {
		return "", err
	}
	return filepath.Join(modelsDir, "manifests", n.host, n.namespace, n.model, n.tag), nil
}

// ModelBlobPath is the file holding tag's weights: the manifest's model
// layer, resolved to its content-addressed blob.
//
// Blobs are shared by digest, so two tags whose weights are byte-identical
// name the SAME file. A caller that edits one is editing every tag that
// points at it, which is why ModelBlobPath reports the digest as well —
// TagsSharingBlob turns it back into the list.
func ModelBlobPath(modelsDir, tag string) (path, digest string, err error) {
	mp, err := ManifestPath(modelsDir, tag)
	if err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(mp)
	if err != nil {
		return "", "", fmt.Errorf("download: read manifest for %s: %w", tag, err)
	}
	var mf struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return "", "", fmt.Errorf("download: parse manifest for %s: %w", tag, err)
	}
	for _, l := range mf.Layers {
		if l.MediaType != mediaTypeModel {
			continue
		}
		name, err := blobFileName(l.Digest)
		if err != nil {
			return "", "", fmt.Errorf("download: %s: %w", tag, err)
		}
		return filepath.Join(modelsDir, "blobs", name), l.Digest, nil
	}
	return "", "", fmt.Errorf("download: %s: %w", tag, ErrNoModelLayer)
}

// blobFileName converts "sha256:<hex>" to the "sha256-<hex>" a blob is
// stored under, refusing anything that would not be a plain file name.
func blobFileName(digest string) (string, error) {
	alg, hex, ok := strings.Cut(digest, ":")
	if !ok || alg == "" || hex == "" || strings.ContainsAny(digest, `/\.`) {
		return "", fmt.Errorf("digest %q is not <alg>:<hex>", digest)
	}
	return alg + "-" + hex, nil
}

// TagsSharingBlob lists every tag in modelsDir whose model layer is digest,
// tag included. It walks the manifest tree, which is small: one small JSON
// per tag the machine has pulled.
//
// It exists because editing a blob edits it for all of them. On the
// reference host every shipped tag had its own weights — even the two
// qwen3.6-35b-a3b builds, which share only their license layer — but a
// family whose plain and suffixed tags resolve to identical weights would
// not, and a caller should know before it writes.
func TagsSharingBlob(modelsDir, digest string) ([]string, error) {
	root := filepath.Join(modelsDir, "manifests")
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil // a manifest we cannot read is not one we can claim shares the blob
		}
		var mf struct {
			Layers []struct {
				MediaType string `json:"mediaType"`
				Digest    string `json:"digest"`
			} `json:"layers"`
		}
		if json.Unmarshal(raw, &mf) != nil {
			return nil
		}
		for _, l := range mf.Layers {
			if l.MediaType == mediaTypeModel && l.Digest == digest {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return nil
				}
				parts := strings.Split(filepath.ToSlash(rel), "/")
				if len(parts) == 4 {
					out = append(out, parts[2]+":"+parts[3])
				}
				break
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("download: walk manifests: %w", err)
	}
	return out, nil
}
