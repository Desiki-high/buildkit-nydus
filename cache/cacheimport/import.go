package cacheimport

import (
	"encoding/json"
	"net/http"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/rootfs"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/blobs"
	"github.com/moby/buildkit/snapshot"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

type ImportOpt struct {
	ContentStore  content.Store
	Snapshotter   snapshot.Snapshotter
	Applier       rootfs.Applier
	CacheAccessor cache.Accessor
}

func NewCacheImporter(opt ImportOpt) *CacheImporter {
	return &CacheImporter{opt: opt}
}

type CacheImporter struct {
	opt ImportOpt
}

func (ci *CacheImporter) pull(ctx context.Context, ref string) (*ocispec.Descriptor, remotes.Fetcher, error) {
	resolver := docker.NewResolver(docker.ResolverOptions{
		Client: http.DefaultClient,
	})

	ref, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	fetcher, err := resolver.Fetcher(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	if _, err := remotes.FetchHandler(ci.opt.ContentStore, fetcher)(ctx, desc); err != nil {
		return nil, nil, err
	}

	return &desc, fetcher, err
}

func (ci *CacheImporter) Import(ctx context.Context, ref string) (InstructionCache, error) {
	desc, fetcher, err := ci.pull(ctx, ref)
	if err != nil {
		return nil, err
	}

	dt, err := content.ReadBlob(ctx, ci.opt.ContentStore, desc.Digest)
	if err != nil {
		return nil, err
	}

	var mfst ocispec.Index
	if err := json.Unmarshal(dt, &mfst); err != nil {
		return nil, err
	}

	allDesc := map[digest.Digest]ocispec.Descriptor{}
	allBlobs := map[digest.Digest]configItem{}
	byCacheKey := map[digest.Digest]configItem{}
	byContentKey := map[digest.Digest][]digest.Digest{}

	var configDesc ocispec.Descriptor

	for _, m := range mfst.Manifests {
		if m.MediaType == mediaTypeConfig {
			configDesc = m
			continue
		}
		allDesc[m.Digest] = m
	}

	if configDesc.Digest == "" {
		return nil, errors.Errorf("invalid build cache: %s", ref)
	}

	if _, err := remotes.FetchHandler(ci.opt.ContentStore, fetcher)(ctx, configDesc); err != nil {
		return nil, err
	}

	dt, err = content.ReadBlob(ctx, ci.opt.ContentStore, configDesc.Digest)
	if err != nil {
		return nil, err
	}

	var cfg cacheConfig
	if err := json.Unmarshal(dt, &cfg); err != nil {
		return nil, err
	}

	for _, ci := range cfg.Items {
		if ci.Blobsum != "" {
			allBlobs[ci.Blobsum] = ci
		}
		if ci.CacheKey != "" {
			byCacheKey[ci.CacheKey] = ci
			if ci.ContentKey != "" {
				byContentKey[ci.ContentKey] = append(byContentKey[ci.ContentKey], ci.CacheKey)
			}
		}
	}

	return &importInfo{
		CacheImporter: ci,
		byCacheKey:    byCacheKey,
		byContentKey:  byContentKey,
		allBlobs:      allBlobs,
		allDesc:       allDesc,
		fetcher:       fetcher,
	}, nil
}

type importInfo struct {
	*CacheImporter
	fetcher      remotes.Fetcher
	byCacheKey   map[digest.Digest]configItem
	byContentKey map[digest.Digest][]digest.Digest
	allDesc      map[digest.Digest]ocispec.Descriptor
	allBlobs     map[digest.Digest]configItem
}

func (ii *importInfo) Probe(ctx context.Context, key digest.Digest) (bool, error) {
	_, ok := ii.byCacheKey[key]
	return ok, nil
}

func (ii *importInfo) getChain(dgst digest.Digest) ([]blobs.DiffPair, error) {
	cfg, ok := ii.allBlobs[dgst]
	if !ok {
		return nil, errors.Errorf("blob %s not found in cache", dgst)
	}
	parent := cfg.Parent

	var out []blobs.DiffPair
	if parent != "" {
		parentChain, err := ii.getChain(parent)
		if err != nil {
			return nil, err
		}
		out = parentChain
	}
	return append(out, blobs.DiffPair{Blobsum: dgst, DiffID: cfg.DiffID}), nil
}

func (ii *importInfo) Lookup(ctx context.Context, key digest.Digest) (interface{}, error) {
	desc, ok := ii.byCacheKey[key]
	if !ok || desc.Blobsum == "" {
		return nil, nil
	}
	ch, err := ii.getChain(desc.Blobsum)
	if err != nil {
		return nil, err
	}
	return ii.fetch(ctx, ch)
}

func (ii *importInfo) Set(key digest.Digest, ref interface{}) error {
	return nil
}

func (ii *importInfo) SetContentMapping(contentKey, key digest.Digest) error {
	return nil
}

func (ii *importInfo) GetContentMapping(dgst digest.Digest) ([]digest.Digest, error) {
	dgsts, ok := ii.byContentKey[dgst]
	if !ok {
		return nil, nil
	}
	return dgsts, nil
}

func (ii *importInfo) fetch(ctx context.Context, chain []blobs.DiffPair) (cache.ImmutableRef, error) {
	eg, ctx := errgroup.WithContext(ctx)
	for _, dp := range chain {
		func(dp blobs.DiffPair) {
			eg.Go(func() error {
				desc, ok := ii.allDesc[dp.Blobsum]
				if !ok {
					return errors.Errorf("failed to find %s for fetch", dp.Blobsum)
				}
				if _, err := remotes.FetchHandler(ii.opt.ContentStore, ii.fetcher)(ctx, desc); err != nil {
					return err
				}
				return nil
			})
		}(dp)
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	chainid, err := ii.unpack(ctx, chain)
	if err != nil {
		return nil, err
	}

	return ii.opt.CacheAccessor.Get(ctx, chainid, cache.WithDescription("imported cache")) // TODO: more descriptive name
}

func (ii *importInfo) unpack(ctx context.Context, dpairs []blobs.DiffPair) (string, error) {
	layers, err := ii.getLayers(ctx, dpairs)
	if err != nil {
		return "", err
	}

	chainID, err := rootfs.ApplyLayers(ctx, layers, ii.opt.Snapshotter, ii.opt.Applier)
	if err != nil {
		return "", err
	}

	if err := ii.fillBlobMapping(ctx, layers); err != nil {
		return "", err
	}

	return string(chainID), nil
}

func (ii *importInfo) fillBlobMapping(ctx context.Context, layers []rootfs.Layer) error {
	var chain []digest.Digest
	for _, l := range layers {
		chain = append(chain, l.Diff.Digest)
		chainID := identity.ChainID(chain)
		if err := ii.opt.Snapshotter.(blobmapper).SetBlob(ctx, string(chainID), l.Diff.Digest, l.Blob.Digest); err != nil {
			return err
		}
	}
	return nil
}

func (ii *importInfo) getLayers(ctx context.Context, dpairs []blobs.DiffPair) ([]rootfs.Layer, error) {
	layers := make([]rootfs.Layer, len(dpairs))
	for i := range dpairs {
		layers[i].Diff = ocispec.Descriptor{
			// TODO: derive media type from compressed type
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    dpairs[i].DiffID,
		}
		info, err := ii.opt.ContentStore.Info(ctx, dpairs[i].Blobsum)
		if err != nil {
			return nil, err
		}
		layers[i].Blob = ocispec.Descriptor{
			// TODO: derive media type from compressed type
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    dpairs[i].Blobsum,
			Size:      info.Size,
		}
	}
	return layers, nil
}

type InstructionCache interface {
	Probe(ctx context.Context, key digest.Digest) (bool, error)
	Lookup(ctx context.Context, key digest.Digest) (interface{}, error) // TODO: regular ref
	Set(key digest.Digest, ref interface{}) error
	SetContentMapping(contentKey, key digest.Digest) error
	GetContentMapping(dgst digest.Digest) ([]digest.Digest, error)
}
