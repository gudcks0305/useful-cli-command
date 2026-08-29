//go:build !darwin

package main

func newIdentityResolver() identityResolver {
	return psIdentityResolver{runner: execPSRunner{}}
}
