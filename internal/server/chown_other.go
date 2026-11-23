//go:build !unix

package server

func chownLike(path, ref string) error { return nil }
