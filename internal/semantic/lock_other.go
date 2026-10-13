//go:build !unix

package semantic

func tryLock(string) func() { return func() {} }
