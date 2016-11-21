package graph

import (
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/digest"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/image"
	"github.com/docker/docker/registry"
)

// LookupRemoteConfig allows you to pass transport-related data to LookupRemote
// function.
type LookupRemoteConfig struct {
	MetaHeaders map[string][]string
	AuthConfig  *cliconfig.AuthConfig
}

// ManifestFetcher allows to pull image's json without any binary blobs.
type ManifestFetcher interface {
	Fetch(ref string) (imgInspect *types.RemoteImageInspect, fallback bool, err error)
}

// NewManifestFetcher creates appropriate fetcher instance for given endpoint.
func NewManifestFetcher(s *TagStore, endpoint registry.APIEndpoint, repoInfo *registry.RepositoryInfo, config *LookupRemoteConfig) (ManifestFetcher, error) {
	switch endpoint.Version {
	case registry.APIVersion2:
		return &v2ManifestFetcher{
			TagStore: s,
			endpoint: endpoint,
			config:   config,
			repoInfo: repoInfo,
		}, nil
	case registry.APIVersion1:
		return &v1ManifestFetcher{
			TagStore: s,
			endpoint: endpoint,
			config:   config,
			repoInfo: repoInfo,
		}, nil
	}
	return nil, fmt.Errorf("unknown version %d for registry %s", endpoint.Version, endpoint.URL)
}

func makeRemoteImageInspect(repoInfo *registry.RepositoryInfo, img *image.Image, tag string, dgst digest.Digest) *types.RemoteImageInspect {
	return &types.RemoteImageInspect{
		types.ImageInspectBase{
			Id:              img.ID,
			Parent:          img.Parent,
			Comment:         img.Comment,
			Created:         img.Created.Format(time.RFC3339Nano),
			Container:       img.Container,
			ContainerConfig: &img.ContainerConfig,
			DockerVersion:   img.DockerVersion,
			Author:          img.Author,
			Config:          img.Config,
			Architecture:    img.Architecture,
			Os:              img.OS,
			Size:            img.Size,
		},
		repoInfo.Index.Name,
		dgst.String(),
		tag,
	}
}

func (s *TagStore) LookupRaw(name string) ([]byte, error) {
	image, err := s.LookupImage(name)
	if err != nil || image == nil {
		return nil, fmt.Errorf("No such image %s", name)
	}

	imageInspectRaw, err := s.graph.RawJSON(image.ID)
	if err != nil {
		return nil, err
	}

	return imageInspectRaw, nil
}

// Lookup return an image encoded in JSON
func (s *TagStore) Lookup(name string) (*types.ImageInspect, error) {
	image, err := s.LookupImage(name)
	if err != nil || image == nil {
		return nil, fmt.Errorf("No such image: %s", name)
	}

	imageInspect := &types.ImageInspect{
		types.ImageInspectBase{
			Id:              image.ID,
			Parent:          image.Parent,
			Comment:         image.Comment,
			Created:         image.Created.Format(time.RFC3339Nano),
			Container:       image.Container,
			ContainerConfig: &image.ContainerConfig,
			DockerVersion:   image.DockerVersion,
			Author:          image.Author,
			Config:          image.Config,
			Architecture:    image.Architecture,
			Os:              image.OS,
			Size:            image.Size,
		},
		s.graph.GetParentsSize(image, 0) + image.Size,
		types.GraphDriverData{
			Name: s.graph.driver.String(),
		},
	}

	imageInspect.GraphDriver.Name = s.graph.driver.String()

	graphDriverData, err := s.graph.driver.GetMetadata(image.ID)
	if err != nil {
		return nil, err
	}
	imageInspect.GraphDriver.Data = graphDriverData
	return imageInspect, nil
}

// LookupRemote returns metadata for remote image.
func (s *TagStore) LookupRemote(name, ref string, config *LookupRemoteConfig) (*types.RemoteImageInspect, error) {
	var (
		imageInspect *types.RemoteImageInspect
		err          error
	)
	// Unless the index name is specified, iterate over all registries until
	// the matching image is found.
	if registry.RepositoryNameHasIndex(name) {
		return s.fetchManifest(name, ref, config)
	}
	if len(registry.RegistryList) == 0 {
		return nil, fmt.Errorf("No configured registry to pull from.")
	}
	for _, r := range registry.RegistryList {
		// Prepend the index name to the image name.
		if imageInspect, err = s.fetchManifest(fmt.Sprintf("%s/%s", r, name), ref, config); err == nil {
			return imageInspect, nil
		}
	}
	return imageInspect, err
}

func (s *TagStore) fetchManifest(name, ref string, config *LookupRemoteConfig) (*types.RemoteImageInspect, error) {
	// Resolve the Repository name from fqn to RepositoryInfo
	repoInfo, err := s.registryService.ResolveRepository(name)
	if err != nil {
		return nil, err
	}

	if err := validateRepoName(repoInfo.LocalName); err != nil {
		return nil, err
	}

	endpoints, err := s.registryService.LookupPullEndpoints(repoInfo.CanonicalName)
	if err != nil {
		return nil, err
	}

	var (
		lastErr error
		// discardNoSupportErrors is used to track whether an endpoint encountered an error of type registry.ErrNoSupport
		// By default it is false, which means that if a ErrNoSupport error is encountered, it will be saved in lastErr.
		// As soon as another kind of error is encountered, discardNoSupportErrors is set to true, avoiding the saving of
		// any subsequent ErrNoSupport errors in lastErr.
		// It's needed for pull-by-digest on v1 endpoints: if there are only v1 endpoints configured, the error should be
		// returned and displayed, but if there was a v2 endpoint which supports pull-by-digest, then the last relevant
		// error is the ones from v2 endpoints not v1.
		discardNoSupportErrors bool
		imgInspect             *types.RemoteImageInspect
	)
	for _, endpoint := range endpoints {
		logrus.Debugf("Trying to fetch image manifest of %s repository from %s %s", repoInfo.CanonicalName, endpoint.URL, endpoint.Version)
		fallback := false

		if !endpoint.Mirror && (endpoint.Official || endpoint.Version == registry.APIVersion2) {
			if repoInfo.Official {
				s.trustService.UpdateBase()
			}
		}

		fetcher, err := NewManifestFetcher(s, endpoint, repoInfo, config)
		if err != nil {
			lastErr = err
			continue
		}
		imgInspect, fallback, err = fetcher.Fetch(ref)
		if err != nil {
			if fallback {
				if _, ok := err.(registry.ErrNoSupport); !ok {
					// Because we found an error that's not ErrNoSupport, discard all subsequent ErrNoSupport errors.
					discardNoSupportErrors = true
					// save the current error
					lastErr = err
				} else if !discardNoSupportErrors {
					// Save the ErrNoSupport error, because it's either the first error or all encountered errors
					// were also ErrNoSupport errors.
					lastErr = err
				}
				continue
			}
			logrus.Debugf("Not continuing with error: %v", err)
			return nil, err
		}

		return imgInspect, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints found for %s", repoInfo.Index.Name)
	}
	return nil, lastErr
}

// ImageTarLayer return the tarLayer of the image
func (s *TagStore) ImageTarLayer(name string, dest io.Writer) error {
	if image, err := s.LookupImage(name); err == nil && image != nil {
		// On Windows, the base layer cannot be exported
		if runtime.GOOS != "windows" || image.Parent != "" {

			fs, err := s.graph.TarLayer(image)
			if err != nil {
				return err
			}
			defer fs.Close()

			written, err := io.Copy(dest, fs)
			if err != nil {
				return err
			}
			logrus.Debugf("rendered layer for %s of [%d] size", image.ID, written)
		}
		return nil
	}
	return fmt.Errorf("No such image: %s", name)
}
