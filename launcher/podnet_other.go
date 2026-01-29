//go:build !linux
// +build !linux

package launcher

import "context"

func (c *clabernetes) capturePodNetSnapshot(_ context.Context) error { return nil }

func (c *clabernetes) ensurePodNetFromSnapshot(_ context.Context) error { return nil }

func (c *clabernetes) startPodNetGuardian() {}
