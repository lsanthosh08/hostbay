//go:build !windows

package main

import "context"

func runService(func(context.Context) error) (bool, error) { return false, nil }
