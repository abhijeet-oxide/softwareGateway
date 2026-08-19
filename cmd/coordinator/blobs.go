package main

import (
	"context"
	"fmt"
	"io"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// blobsImpl reads one blob of a release out of the registry that published it.
//
// The composition root again, and for the same reason as compareImpl: reaching
// a vendor registry means the client factory and the credentials behind it,
// which nothing under internal/api may hold.
//
// It answers for exactly one thing and takes no digest it was not given
// alongside the package the digest belongs to. THE CALLER HAS ALREADY
// ESTABLISHED THAT - see store.FileInPackage - and this deliberately cannot
// re-open that question, which is what keeps the endpoint from being a proxy
// for arbitrary blobs on a credentialed connection.
type blobsImpl struct {
	*resolverImpl
}

// ReadBlob opens a blob from the source the package was discovered in.
//
// The SOURCE, never a target. What a target holds is a question a comparison
// asks; what the vendor published is what a reader opening a file means, and
// they are the same bytes exactly when nothing has gone wrong - which is not
// something to assume while showing somebody a file.
func (b blobsImpl) ReadBlob(
	ctx context.Context, productName string, pkg store.PackageRow, digest string,
) (io.ReadCloser, error) {
	p, ok := b.products.Get(productName)
	if !ok {
		return nil, fmt.Errorf("product %q is not configured", productName)
	}
	if pkg.SourceRepository == "" {
		return nil, fmt.Errorf(
			"release %s has no source repository recorded, so there is nowhere to read it from",
			pkg.Tag)
	}

	source, err := findSource(p, "")
	if err != nil {
		return nil, err
	}

	repo, err := b.clients.For(v1.JobEndpoint{
		Product:    productName,
		Name:       source.Name,
		Registry:   source.Registry,
		Repository: pkg.SourceRepository,
		Type:       string(source.Type),
		Role:       string(product.RoleSource),
	})
	if err != nil {
		return nil, err
	}
	return repo.FetchBlob(ctx, registry.Digest(digest))
}
