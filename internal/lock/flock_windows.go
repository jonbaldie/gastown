//go:build windows

package lock

// FlockAcquire is a no-op on Windows. Gas Town doesn't run on Windows
// in production, so the advisory lock is not critical here.
func FlockAcquire(_ string) (func(), error) {
	return func() {}, nil
}

// flockAcquire is a no-op on Windows. Gas Town doesn't run on Windows
// in production, so the advisory lock is not critical here.
func flockAcquire(_ string) (func(), error) {
	return func() {}, nil
}

// FlockTryAcquire is a no-op on Windows. Always reports success.
func FlockTryAcquire(_ string) (func(), bool, error) {
	return func() {}, true, nil
}
