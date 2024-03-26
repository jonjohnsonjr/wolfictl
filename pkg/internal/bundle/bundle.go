package bundle

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"

	"chainguard.dev/apko/pkg/build/types"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/wolfi-dev/wolfictl/pkg/dag"
)

// todo: optimize this if it matters (it probably doesn't)
func layer(fsys fs.FS) (v1.Layer, error) {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	if err := tw.AddFS(fsys); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	opener := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	}

	return tarball.LayerFromOpener(opener)
}

func New(config *dag.Configuration, base v1.ImageIndex, archs []string, srcfs fs.FS) (v1.ImageIndex, error) {
	m, err := base.IndexManifest()
	if err != nil {
		return nil, err
	}

	wantArchs := map[string]struct{}{}
	for _, arch := range archs {
		wantArchs[types.ParseArchitecture(arch).ToAPK()] = struct{}{}
	}

	var idx v1.ImageIndex = empty.Index

	for _, desc := range m.Manifests {
		arch := types.ParseArchitecture(desc.Platform.Architecture).ToAPK()
		if _, ok := wantArchs[arch]; !ok {
			continue
		}

		baseImg, err := base.Image(desc.Digest)
		if err != nil {
			return nil, err
		}

		layer, err := layer(srcfs)
		if err != nil {
			return nil, err
		}

		img, err := mutate.AppendLayers(baseImg, layer)
		if err != nil {
			return nil, err
		}

		newDesc, err := partial.Descriptor(img)
		if err != nil {
			return nil, err
		}

		newDesc.Platform = desc.Platform

		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: *newDesc,
		})
	}

	return idx, nil
}

// Podspec returns bytes of yaml representing a podspec.
// This is a terrible API that we should change.
func Podspec(config *dag.Configuration, ref name.Reference) ([]byte, error) {
}
