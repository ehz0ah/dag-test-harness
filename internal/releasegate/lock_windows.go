//go:build windows

package releasegate

import (
	"context"
	"sync"
)

var sourceCacheMutex sync.Mutex

func lockSourceCache(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sourceCacheMutex.Lock()
	return sourceCacheMutex.Unlock, nil
}
