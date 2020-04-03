/*
   Copyright The containerd Authors.

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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package remote

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/reference/docker"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/pkg/errors"
)

const (
	defaultChunkSize     = 50000
	defaultValidInterval = 60 * time.Second
)

type ResolverConfig struct {
	Mirrors []MirrorConfig `toml:"mirrors"`
}

type MirrorConfig struct {
	Host     string `toml:"host"`
	Insecure bool   `toml:"insecure"`
}

type BlobConfig struct {
	ValidInterval int64 `toml:"valid_interval"`
	CheckAlways   bool  `toml:"check_always"`
	ChunkSize     int64 `toml:"chunk_size"`
}

func NewResolver(keychain authn.Keychain, config map[string]ResolverConfig) *Resolver {
	if config == nil {
		config = make(map[string]ResolverConfig)
	}
	return &Resolver{
		transport: http.DefaultTransport,
		trPool:    make(map[string]http.RoundTripper),
		keychain:  keychain,
		config:    config,
	}
}

type Resolver struct {
	transport http.RoundTripper
	trPool    map[string]http.RoundTripper
	trPoolMu  sync.Mutex
	keychain  authn.Keychain
	config    map[string]ResolverConfig
}

func (r *Resolver) Resolve(ref, digest string, cache cache.BlobCache, config BlobConfig) (*Blob, error) {
	// Get blob information
	var (
		nref name.Reference
		url  string
		tr   http.RoundTripper
		size int64
	)
	named, err := docker.ParseDockerRef(ref)
	if err != nil {
		return nil, err
	}
	hosts := append(r.config[docker.Domain(named)].Mirrors, MirrorConfig{
		Host: docker.Domain(named),
	})
	var rErr error
	for _, h := range hosts {
		// Parse reference
		if h.Host == "" || strings.Contains(h.Host, "/") {
			rErr = errors.Wrapf(rErr, "host %q: mirror must be a domain name", h.Host)
			continue // try another host
		}
		var opts []name.Option
		if h.Insecure {
			opts = append(opts, name.Insecure)
		}
		sref := fmt.Sprintf("%s/%s", h.Host, docker.Path(named))
		nref, err = name.ParseReference(sref, opts...)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to parse ref %q (%q): %v",
				h.Host, sref, digest, err)
			continue // try another host
		}

		// Resolve redirection and get blob URL
		url, err = r.resolveReference(nref, digest)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to resolve ref %q (%q): %v",
				h.Host, nref.String(), digest, err)
			continue // try another host
		}

		// Get authenticated RoundTripper
		r.trPoolMu.Lock()
		tr = r.trPool[nref.Name()]
		r.trPoolMu.Unlock()
		if tr == nil {
			rErr = errors.Wrapf(rErr, "host %q: transport %q not found in pool",
				h.Host, nref.Name())
			continue
		}

		// Get size information
		size, err = getSize(url, tr)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to get size of %q: %v",
				h.Host, url, err)
			continue // try another host
		}

		rErr = nil // Hit one accessible mirror
		break
	}
	if rErr != nil {
		return nil, errors.Wrapf(rErr, "cannot resolve ref %q (%q)", ref, digest)
	}

	// Configure the connection
	var (
		chunkSize     int64
		checkInterval time.Duration
	)
	chunkSize = config.ChunkSize
	if chunkSize == 0 { // zero means "use default chunk size"
		chunkSize = defaultChunkSize
	}
	if config.ValidInterval == 0 { // zero means "use default interval"
		checkInterval = defaultValidInterval
	} else {
		checkInterval = time.Duration(config.ValidInterval) * time.Second
	}
	if config.CheckAlways {
		checkInterval = 0
	}

	return &Blob{
		ref:           nref,
		keychain:      r.keychain,
		url:           url,
		tr:            tr,
		size:          size,
		chunkSize:     chunkSize,
		cache:         cache,
		checkInterval: checkInterval,
		lastCheck:     time.Now(),
	}, nil
}

func (r *Resolver) resolveReference(ref name.Reference, digest string) (string, error) {
	r.trPoolMu.Lock()
	defer r.trPoolMu.Unlock()

	// Construct endpoint URL from given ref
	endpointURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s",
		ref.Context().Registry.Scheme(),
		ref.Context().RegistryStr(),
		ref.Context().RepositoryStr(),
		digest)

	// Try to use cached transport (cahced per reference name)
	if tr, ok := r.trPool[ref.Name()]; ok {
		if url, err := redirect(endpointURL, tr); err == nil {
			return url, nil
		}
	}

	// transport is unavailable/expired so refresh the transport and try again
	tr, err := authnTransport(ref, r.transport, r.keychain)
	if err != nil {
		return "", err
	}
	url, err := redirect(endpointURL, tr)
	if err != nil {
		return "", err
	}

	// Update transports cache
	r.trPool[ref.Name()] = tr

	return url, nil
}

func redirect(endpointURL string, tr http.RoundTripper) (url string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// We use GET request for GCR.
	req, err := http.NewRequest("GET", endpointURL, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to request to the registry of %q", endpointURL)
	}
	req = req.WithContext(ctx)
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := tr.RoundTrip(req)
	if err != nil {
		return "", errors.Wrapf(err, "failed to request to %q", endpointURL)
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode/100 == 2 {
		url = endpointURL
	} else if redir := res.Header.Get("Location"); redir != "" && res.StatusCode/100 == 3 {
		// TODO: Support nested redirection
		url = redir
	} else {
		return "", fmt.Errorf("failed to access to %q with code %v",
			endpointURL, res.StatusCode)
	}

	return
}

func getSize(url string, tr http.RoundTripper) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req = req.WithContext(ctx)
	req.Close = false
	res, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed HEAD request with code %v", res.StatusCode)
	}
	return strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
}

func authnTransport(ref name.Reference, tr http.RoundTripper, keychain authn.Keychain) (http.RoundTripper, error) {
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	auth, err := keychain.Resolve(ref.Context())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve the reference %q", ref)
	}
	return transport.New(ref.Context().Registry, auth, tr, []string{ref.Scope(transport.PullScope)})
}
