// Package warmup is the function the build image compiles once, so that the
// runtime and the standard library it uses are in the image's build cache.
package warmup

import (
	"context"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

// Handle answers nothing.
func Handle(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
	return nil, nil
}
